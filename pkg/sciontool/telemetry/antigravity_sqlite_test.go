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
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
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
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
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
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
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
			DetectVersion:     func(context.Context) (string, error) { return "agy 1.5.18.1", nil },
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
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
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
		DetectVersion: func(context.Context) (string, error) { return "1.5.18", nil },
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

func TestDetectAgyVersionManaged_IgnoresAgentPATHHijack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix PATH hijack test")
	}
	poisonDir := t.TempDir()
	marker := filepath.Join(poisonDir, "pwned")
	fakeAgy := filepath.Join(poisonDir, "agy")
	script := "#!/bin/sh\ntouch " + marker + "\necho 9.9.9\n"
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", poisonDir+":"+os.Getenv("PATH"))

	_, _ = detectAgyVersionManaged(context.Background())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("detectAgyVersionManaged executed untrusted agy from PATH")
	}
}

func TestAntigravitySQLiteTailer_R1_CrashBeforePersistReplayIdempotent(t *testing.T) {
	cases := []struct {
		name  string
		usage testAgyUsage
		ts    uint64
	}{
		{
			name:  "request_id_nil_timestamp",
			usage: testAgyUsage{modelID: 1466, input: 500, output: 100, id11: "req-nil-ts"},
			ts:    0,
		},
		{
			name:  "composite_keyless_identity",
			usage: testAgyUsage{modelID: 1466, input: 500, output: 100},
			ts:    1795704000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTestAgyEnv(t)
			home := t.TempDir()
			dbPath := filepath.Join(AgyConversationsDir(home), "conv-crash.db")
			db := createTestAgyDB(t, dbPath)
			defer func() { _ = db.Close() }()

			gen0 := encodeTestGenMeta(1466, "gemini-4-neon", tc.ts, tc.usage)
			if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
				t.Fatalf("insert gen0: %v", err)
			}

			outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
			if err != nil {
				t.Fatalf("open outbox: %v", err)
			}
			analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

			t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
			tailer1 := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
				AgentHome:         home,
				DisableWakeSocket: true,
				Analytics:         analytics,
				DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
				Now:               func() time.Time { return t0 },
			})
			if err := tailer1.PollOnce(context.Background()); err != nil {
				t.Fatalf("poll 1: %v", err)
			}

			// Simulate crash after outbox append and before state persist by removing state file.
			if err := os.Remove(AgySQLiteStatePath(home)); err != nil {
				t.Fatalf("remove state file: %v", err)
			}

			// Replay with a fresh tailer at a later wall-clock time.
			t1 := t0.Add(3 * time.Minute)
			var replaySessionInput int64
			tailer2 := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
				AgentHome:         home,
				DisableWakeSocket: true,
				Analytics:         analytics,
				DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
				Now:               func() time.Time { return t1 },
				SessionUsage: func(u SessionUsage) {
					replaySessionInput += u.TokensInput
				},
			})
			if err := tailer2.PollOnce(context.Background()); err != nil {
				t.Fatalf("poll 2 (replay): %v", err)
			}
			if replaySessionInput != 0 {
				t.Fatalf("duplicate replay re-added session input tokens = %d, want 0 (O5)", replaySessionInput)
			}

			events := drainOutboxEvents(t, outbox)
			if len(events) != 1 {
				t.Fatalf("got %d events after crash replay, want exactly 1", len(events))
			}
			if events[0].Input == nil || *events[0].Input != 500 || events[0].Output == nil || *events[0].Output != 100 {
				t.Fatalf("unexpected totals: in=%v out=%v, want 500/100", events[0].Input, events[0].Output)
			}
		})
	}
}

func TestAntigravitySQLiteTailer_R1_TwoRecordPartialAppendFailureRetry(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-partial.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	// Record 0: composite (keyless) with nil timestamp.
	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 0, testAgyUsage{
		modelID: 1466, input: 300, output: 50,
	})
	// Record 1: request_id with nil timestamp.
	gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 0, testAgyUsage{
		modelID: 1466, input: 700, output: 150, id11: "partial-req-2",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?), (1, ?, ?)`, gen0, len(gen0), gen1, len(gen1)); err != nil {
		t.Fatalf("insert gen_metadata: %v", err)
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

	// Fail ONLY the 2nd record append on poll 1 (record 0 succeeds durably).
	callNum := 0
	analytics.setRunAndActivity("run-agy-test", func(time.Time) {
		callNum++
		if callNum == 2 {
			_ = os.Setenv("SCION_AGENT_ID", "")
		}
	})

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         analytics,
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		Now:               func() time.Time { return now },
	})

	if err := tailer.PollOnce(context.Background()); err == nil {
		t.Fatal("expected PollOnce to fail on 2nd record append")
	}

	// Restore agent identity and advance wall clock; retry must skip already-appended
	// record 0 as a certain duplicate and append record 1 exactly once.
	t.Setenv("SCION_AGENT_ID", "agent-agy-test")
	analytics.setRunAndActivity("run-agy-test", nil)
	now = now.Add(2 * time.Minute)

	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("retry PollOnce: %v", err)
	}

	events := drainOutboxEvents(t, outbox)
	if len(events) != 2 {
		t.Fatalf("got %d events after partial-failure retry, want exactly 2", len(events))
	}
	seenIDs := make(map[string]bool)
	var totIn, totOut int64
	for _, ev := range events {
		if seenIDs[ev.EventID] {
			t.Fatalf("duplicate event_id %q", ev.EventID)
		}
		seenIDs[ev.EventID] = true
		if ev.Input != nil {
			totIn += *ev.Input
		}
		if ev.Output != nil {
			totOut += *ev.Output
		}
	}
	if totIn != 1000 || totOut != 200 {
		t.Fatalf("totals = (in=%d out=%d), want (1000, 200)", totIn, totOut)
	}
}

func TestAntigravitySQLiteTailer_R2_AtomicStateWriteDoesNotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test is unix-only")
	}
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-sym.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795705000, testAgyUsage{
		modelID: 1466, input: 100, output: 20, id11: "sym-call-1",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	// Plant a symlink at agy-sqlite-state.json pointing to a victim file.
	victimPath := filepath.Join(home, "victim.txt")
	if err := os.WriteFile(victimPath, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	statePath := AgySQLiteStatePath(home)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}
	if err := os.Symlink(victimPath, statePath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	// Victim target must be completely untouched.
	victimBytes, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(victimBytes) != "untouched" {
		t.Fatalf("symlink target was modified: got %q, want \"untouched\"", victimBytes)
	}
	// State path must now be a regular 0600 file containing valid JSON, not a symlink.
	lfi, err := os.Lstat(statePath)
	if err != nil {
		t.Fatalf("lstat state path: %v", err)
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("state path is still a symlink; expected atomic rename replacement")
	}
	if lfi.Mode().Perm() != 0o600 {
		t.Fatalf("state file perm = %o, want 0600", lfi.Mode().Perm())
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var parsed agySQLitePersistedState
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("persisted files = %d, want 1", len(parsed.Files))
	}
}

func TestAntigravitySQLiteTailer_R3_RetryableVersionDetectionAndRunScopedLatch(t *testing.T) {
	t.Run("timeout_then_success_emits_tokens", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-ver.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795706000, testAgyUsage{
			modelID: 1466, input: 450, output: 90, id11: "ver-retry-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open outbox: %v", err)
		}
		attempts := 0
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
			DetectVersion: func(context.Context) (string, error) {
				attempts++
				if attempts == 1 {
					return "", context.DeadlineExceeded
				}
				return "1.5.18", nil
			},
		})

		// Poll 1: transient version detection timeout must NOT latch drift or advance cursor.
		if err := tailer.PollOnce(context.Background()); err == nil {
			t.Fatal("expected PollOnce error on version detection timeout")
		}
		if tailer.SchemaDriftDetected() {
			t.Fatal("transient version detection failure must not latch schema drift")
		}
		if got := drainOutboxEvents(t, outbox); len(got) != 0 {
			t.Fatalf("expected 0 events after timeout, got %d", len(got))
		}

		// Poll 2: detection succeeds -> tokens for those rows are emitted.
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 2: %v", err)
		}
		if tailer.SchemaDriftDetected() {
			t.Fatal("unexpected drift latch after verified version")
		}
		events := drainOutboxEvents(t, outbox)
		if len(events) != 1 || events[0].Input == nil || *events[0].Input != 450 {
			t.Fatalf("got events %+v, want 1 event with input=450", events)
		}
	})

	t.Run("same_run_stays_latched_new_run_unlatches", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-latch.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795707000, testAgyUsage{
			modelID: 1466, input: 100, output: 10, id11: "latch-call-0",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open outbox: %v", err)
		}
		analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

		// Run A sees unverified version -> latches drift and persists drift_run_id="run-A".
		t.Setenv("SCION_LAUNCH_ID", "run-A")
		tailerA1 := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			RunID:             "run-A",
			DisableWakeSocket: true,
			Analytics:         analytics,
			DetectVersion:     func(context.Context) (string, error) { return "9.9.9", nil },
		})
		if err := tailerA1.PollOnce(context.Background()); err != nil {
			t.Fatalf("run-A poll 1: %v", err)
		}
		if !tailerA1.SchemaDriftDetected() {
			t.Fatal("expected run-A to latch drift")
		}
		_ = drainOutboxEvents(t, outbox) // consume drift sentinel

		// Restart within the SAME run ("run-A"): latch must persist even if version is now verified.
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795707005, testAgyUsage{
			modelID: 1466, input: 200, output: 20, id11: "latch-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		tailerA2 := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			RunID:             "run-A",
			DisableWakeSocket: true,
			Analytics:         analytics,
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		})
		if !tailerA2.SchemaDriftDetected() {
			t.Fatal("expected drift latch to persist across restart within same run-A")
		}
		if err := tailerA2.PollOnce(context.Background()); err != nil {
			t.Fatalf("run-A restart poll: %v", err)
		}
		if got := drainOutboxEvents(t, outbox); len(got) != 0 {
			t.Fatalf("same-run restart while latched must emit 0 token events, got %d", len(got))
		}

		// New run ("run-B") with verified version: latch must reset and emit tokens for new rows.
		t.Setenv("SCION_LAUNCH_ID", "run-B")
		gen2 := encodeTestGenMeta(1466, "gemini-4-neon", 1795707010, testAgyUsage{
			modelID: 1466, input: 333, output: 44, id11: "latch-call-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (2, ?, ?)`, gen2, len(gen2)); err != nil {
			t.Fatalf("insert gen2: %v", err)
		}
		tailerB := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			RunID:             "run-B",
			DisableWakeSocket: true,
			Analytics:         analytics,
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		})
		if tailerB.SchemaDriftDetected() {
			t.Fatal("expected new run-B to un-latch persisted drift from run-A")
		}
		if err := tailerB.PollOnce(context.Background()); err != nil {
			t.Fatalf("run-B poll: %v", err)
		}
		events := drainOutboxEvents(t, outbox)
		if len(events) != 1 || events[0].Input == nil || *events[0].Input != 333 {
			t.Fatalf("run-B got events %+v, want 1 event with input=333", events)
		}
	})
}

func TestAntigravitySQLiteTailer_O2_NilAnalyticsDoesNotCommitCursors(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-nosink.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795708000, testAgyUsage{
		modelID: 1466, input: 100, output: 20, id11: "nosink-1",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         nil,
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce with nil analytics: %v", err)
	}
	if _, err := os.Stat(AgySQLiteStatePath(home)); !os.IsNotExist(err) {
		t.Fatalf("expected no state file committed when Analytics is nil, got err=%v", err)
	}
}

func TestPipeline_O3_DriftLatchPreservedAfterTailerCleared(t *testing.T) {
	setTestAgyEnv(t)
	p := NewWithConfig(&Config{Enabled: true})
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         t.TempDir(),
		DisableWakeSocket: true,
	})
	tailer.driftLatched.Store(true)
	p.agyTailer.Store(tailer)

	// Simulate Pipeline.Stop capturing the latch before nilling agyTailer.
	if stopped := p.agyTailer.Swap(nil); stopped != nil && stopped.SchemaDriftDetected() {
		p.driftLatched.Store(true)
	}
	if !p.schemaDriftDetected() {
		t.Fatal("expected schemaDriftDetected() == true after agyTailer cleared")
	}
	sigs := p.supportedUsageSignals()
	if len(sigs) != 1 || sigs[0] != "calls" {
		t.Fatalf("supportedUsageSignals = %v, want [calls]", sigs)
	}
}

func TestAntigravitySQLiteTailer_Rev2_DualSinkHubAndGCPFailureReplay(t *testing.T) {
	cases := []struct {
		name    string
		failHub bool
	}{
		{name: "hub_fails_then_recovers", failHub: true},
		{name: "gcp_fails_then_recovers", failHub: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTestAgyEnv(t)
			home := t.TempDir()
			dbPath := filepath.Join(AgyConversationsDir(home), "conv-dual.db")
			db := createTestAgyDB(t, dbPath)
			defer func() { _ = db.Close() }()

			gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795709000, testAgyUsage{
				modelID: 1466, input: 600, output: 140, id11: "dual-call-1",
			})
			if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
				t.Fatalf("insert gen0: %v", err)
			}

			gcpOutbox, err := OpenAnalyticsOutbox(AnalyticsOutboxDirFor(home))
			if err != nil {
				t.Fatalf("open gcp outbox: %v", err)
			}
			hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
			if err != nil {
				t.Fatalf("open hub outbox: %v", err)
			}
			dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceSQLiteNative)

			var sessionInput int64
			tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
				AgentHome:         home,
				DisableWakeSocket: true,
				Analytics:         dual,
				DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
				SessionUsage: func(u SessionUsage) {
					sessionInput += u.TokensInput
				},
			})

			// Inject failure on one sink during poll 1.
			if tc.failHub {
				hubOutbox.crashAt = "intent"
			} else {
				gcpOutbox.crashAt = "intent"
			}
			if err := tailer.PollOnce(context.Background()); err == nil {
				t.Fatal("expected PollOnce to return error when one dual sink fails")
			}
			if sessionInput != 0 {
				t.Fatalf("session input = %d after failed poll, want 0", sessionInput)
			}

			// Clear fault and retry: surviving sink deduplicates as AppendCertainDuplicate,
			// failed sink appends once, cursor commits, and session usage adds once.
			hubOutbox.crashAt = ""
			gcpOutbox.crashAt = ""
			if err := tailer.PollOnce(context.Background()); err != nil {
				t.Fatalf("retry PollOnce: %v", err)
			}

			gcpEvents := drainOutboxEvents(t, gcpOutbox)
			hubEvents := drainOutboxEvents(t, hubOutbox)
			if len(gcpEvents) != 1 || len(hubEvents) != 1 {
				t.Fatalf("got gcp=%d hub=%d events, want 1 and 1", len(gcpEvents), len(hubEvents))
			}
			if gcpEvents[0].SourceEventID != hubEvents[0].SourceEventID {
				t.Fatalf("source_event_id mismatch: gcp=%q hub=%q", gcpEvents[0].SourceEventID, hubEvents[0].SourceEventID)
			}
			if hubEvents[0].Input == nil || *hubEvents[0].Input != 600 || hubEvents[0].Output == nil || *hubEvents[0].Output != 140 {
				t.Fatalf("hub tokens = (%v, %v), want (600, 140)", hubEvents[0].Input, hubEvents[0].Output)
			}
			if sessionInput != 600 {
				t.Fatalf("session input = %d, want 600 (counted exactly once)", sessionInput)
			}
		})
	}
}

func TestStartAgySQLiteWakeListener_RefusesSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix wake socket symlink test")
	}
	home := t.TempDir()
	targetDir := t.TempDir()
	scionLink := filepath.Join(home, ".scion")
	if err := os.Symlink(targetDir, scionLink); err != nil {
		t.Fatalf("symlink .scion: %v", err)
	}
	closer, err := startAgySQLiteWakeListener(AgySQLiteWakePath(home), nil, func() {})
	if err == nil {
		_ = closer.Close()
		t.Fatal("expected startAgySQLiteWakeListener to refuse symlinked .scion parent")
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("readdir targetDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target dir was modified: %v", entries)
	}
}

func TestAntigravitySQLiteTailer_Rev2_VersionDetectOncePerPollAndLatchAfterN(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	for _, name := range []string{"conv-a.db", "conv-b.db"} {
		dbPath := filepath.Join(AgyConversationsDir(home), name)
		db := createTestAgyDB(t, dbPath)
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795709500, testAgyUsage{
			modelID: 1466, input: 100, output: 20, id11: "vfail-" + name,
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			_ = db.Close()
			t.Fatalf("insert %s: %v", name, err)
		}
		_ = db.Close()
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	detectCalls := 0
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:          home,
		DisableWakeSocket:  true,
		Analytics:          NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		MaxVersionFailures: 3,
		DetectVersion: func(context.Context) (string, error) {
			detectCalls++
			return "", context.DeadlineExceeded
		},
	})

	// Polls 1 and 2: transient retries (1 detection call per poll across 2 DBs, no drift latch yet).
	for i := 1; i <= 2; i++ {
		if err := tailer.PollOnce(context.Background()); err == nil {
			t.Fatalf("poll %d: expected error", i)
		}
		if detectCalls != i {
			t.Fatalf("poll %d: detectCalls = %d, want %d (once per poll, not per file)", i, detectCalls, i)
		}
		if tailer.SchemaDriftDetected() {
			t.Fatalf("poll %d: must not latch drift before MaxVersionFailures", i)
		}
	}

	// Poll 3 hits MaxVersionFailures -> latches drift and emits unverified sentinel.
	if err := tailer.PollOnce(context.Background()); err == nil {
		t.Fatal("poll 3: expected error")
	}
	if detectCalls != 3 {
		t.Fatalf("poll 3: detectCalls = %d, want 3", detectCalls)
	}
	if !tailer.SchemaDriftDetected() {
		t.Fatal("poll 3: expected SchemaDriftDetected() == true after 3 consecutive failures")
	}
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 || events[0].TokenStatus != telemetrycontract.TokenStatusUnverified || events[0].Calls != 0 || events[0].Input != nil {
		t.Fatalf("expected 1 unverified drift sentinel with calls=0 and nil tokens, got %+v", events)
	}
}

func TestPipeline_Rev2_ActivateSQLiteNativeWithoutAnalytics(t *testing.T) {
	t.Setenv("SCION_HARNESS", "antigravity")
	t.Setenv("SCION_USAGE_SOURCE", "")
	if err := os.Unsetenv("SCION_USAGE_SOURCE"); err != nil {
		t.Fatalf("unset SCION_USAGE_SOURCE: %v", err)
	}

	cfg := &Config{
		Enabled:      true,
		GRPCPort:     0,
		HTTPPort:     0,
		CloudEnabled: false,
	}
	p := NewWithConfig(cfg)
	p.SetAnalyticsOutbox(t.TempDir(), 0, 0)
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	defer func() { _ = p.Stop(ctx) }()

	outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative)
	if err != nil {
		t.Fatalf("ActivateUsageSource: %v", err)
	}
	if outcome != UsageActivationAnalyticsDisabled {
		t.Fatalf("ActivateUsageSource outcome = %q, want %q", outcome, UsageActivationAnalyticsDisabled)
	}
	if p.agyTailer.Load() != nil {
		t.Fatal("expected agyTailer to remain nil when analytics is disabled")
	}
}
