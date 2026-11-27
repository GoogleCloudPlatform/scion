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
	"errors"
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

// seedEmptyAgyState writes an initialized empty agy-sqlite-state.json so tests
// that pre-populate a DB before constructing a tailer exercise decoding rather
// than first-start historical baselining.
func seedEmptyAgyState(t *testing.T, home string) {
	t.Helper()
	p := AgySQLiteStatePath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"version":1}`+"\n"), 0o600); err != nil {
		t.Fatalf("write empty state: %v", err)
	}
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

	seedEmptyAgyState(t, home)
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         analytics,
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})

	// Baseline empty DB before round 1.
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("baseline poll: %v", err)
	}

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

	seedEmptyAgyState(t, home)
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
		seedEmptyAgyState(t, home)
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
		seedEmptyAgyState(t, home)
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

			seedEmptyAgyState(t, home)
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

			// Simulate crash after outbox append and before state persist by restoring
			// the pre-poll empty state file so tailer2 replays the exact same DB row.
			seedEmptyAgyState(t, home)

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
	seedEmptyAgyState(t, home)
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
		seedEmptyAgyState(t, home)
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
		seedEmptyAgyState(t, home)
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
			seedEmptyAgyState(t, home)
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
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	seedEmptyAgyState(t, home)
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:               home,
		DisableWakeSocket:       true,
		Analytics:               NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		MaxVersionFailures:      3,
		MinVersionFailureWindow: 5 * time.Minute,
		Now:                     func() time.Time { return now },
		DetectVersion: func(context.Context) (string, error) {
			detectCalls++
			return "", context.DeadlineExceeded
		},
	})

	// Polls 1..3 within 2 minutes (< 5m window): must NOT latch drift yet, and
	// must call DetectVersion once per poll (not once per DB file).
	for i := 1; i <= 3; i++ {
		if err := tailer.PollOnce(context.Background()); err == nil {
			t.Fatalf("poll %d: expected error", i)
		}
		if detectCalls != i {
			t.Fatalf("poll %d: detectCalls = %d, want %d (once per poll, not per file)", i, detectCalls, i)
		}
		if tailer.SchemaDriftDetected() {
			t.Fatalf("poll %d: must not latch drift before MinVersionFailureWindow elapses", i)
		}
		now = now.Add(1 * time.Minute)
	}

	// Poll 4 at t+5m (>= 3 failures AND >= 5m elapsed): latches drift and emits unverified sentinel.
	now = now.Add(2 * time.Minute)
	if err := tailer.PollOnce(context.Background()); err == nil {
		t.Fatal("poll 4: expected error")
	}
	if detectCalls != 4 {
		t.Fatalf("poll 4: detectCalls = %d, want 4", detectCalls)
	}
	if !tailer.SchemaDriftDetected() {
		t.Fatal("poll 4: expected SchemaDriftDetected() == true after >=3 failures spanning >=5m")
	}
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 || events[0].TokenStatus != telemetrycontract.TokenStatusUnverified || events[0].Calls != 0 || events[0].Input != nil {
		t.Fatalf("expected 1 unverified drift sentinel with calls=0 and nil tokens, got %+v", events)
	}

	// Poll 5 after latching (with a new DB write): must NOT re-run DetectVersion!
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-a.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open conv-a.db: %v", err)
	}
	gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795709600, testAgyUsage{
		modelID: 1466, input: 555, output: 66, id11: "post-latch-row",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
		_ = db.Close()
		t.Fatalf("insert gen1: %v", err)
	}
	_ = db.Close()

	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll 5 (latched): %v", err)
	}
	if detectCalls != 4 {
		t.Fatalf("poll 5 re-ran DetectVersion after latch: detectCalls = %d, want 4", detectCalls)
	}
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("poll 5 (latched) emitted %d events, want 0", len(got))
	}
}

func TestAntigravitySQLiteTailer_Rev3_OverflowHoldsCursorAndReplaysAfterDrain(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-overflow.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795710000, testAgyUsage{
		modelID: 1466, input: 888, output: 99, id11: "overflow-call-1",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open hub outbox: %v", err)
	}
	// Tiny pending budget so the first append overflows.
	hubOutbox.maxPending = 128

	seedEmptyAgyState(t, home)
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})

	// Poll 1: overflows -> returns ErrAnalyticsOutboxOverflow and holds cursor.
	err = tailer.PollOnce(context.Background())
	if !errors.Is(err, ErrAnalyticsOutboxOverflow) {
		t.Fatalf("poll 1 err = %v, want ErrAnalyticsOutboxOverflow", err)
	}
	if got := drainOutboxEvents(t, hubOutbox); len(got) != 0 {
		t.Fatalf("expected 0 events after overflow, got %d", len(got))
	}

	// Free space (simulate drain completing) and poll again -> tokens arrive once.
	hubOutbox.maxPending = analyticsOutboxMaxPending
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll 2 after drain: %v", err)
	}
	events := drainOutboxEvents(t, hubOutbox)
	if len(events) != 1 {
		t.Fatalf("got %d events after replay, want 1", len(events))
	}
	if events[0].Input == nil || *events[0].Input != 888 || events[0].Output == nil || *events[0].Output != 99 {
		t.Fatalf("tokens = (%v, %v), want (888, 99)", events[0].Input, events[0].Output)
	}
}

func TestAntigravitySQLiteTailer_Rev3_FirstStartBaselinesExistingHistory(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-hist.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	// Pre-populate historical row BEFORE the fresh tailer starts (no state file).
	histRow := encodeTestGenMeta(1466, "gemini-4-neon", 1795700000, testAgyUsage{
		modelID: 1466, input: 9999, output: 8888, id11: "historical-call",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, histRow, len(histRow)); err != nil {
		t.Fatalf("insert historical row: %v", err)
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

	// Poll 1 with no state file: baselines existing DB, emits 0 events.
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("baseline PollOnce: %v", err)
	}
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("first-start baseline emitted %d events, want 0", len(got))
	}

	// Insert a new row for the current run -> emitted exactly once.
	newRow := encodeTestGenMeta(1466, "gemini-4-neon", 1795711000, testAgyUsage{
		modelID: 1466, input: 321, output: 65, id11: "current-run-call",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, newRow, len(newRow)); err != nil {
		t.Fatalf("insert new row: %v", err)
	}
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("post-baseline PollOnce: %v", err)
	}
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 {
		t.Fatalf("got %d events for new row, want 1", len(events))
	}
	if events[0].Input == nil || *events[0].Input != 321 || events[0].Output == nil || *events[0].Output != 65 {
		t.Fatalf("tokens = (%v, %v), want (321, 65)", events[0].Input, events[0].Output)
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

func TestPipeline_AgyTailerWithCloudInitDegradedAndNativeMeteringOnly(t *testing.T) {
	t.Cleanup(SetTelemetryTestSandboxed())
	setTestAgyEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_USAGE_SOURCE", "")
	if err := os.Unsetenv("SCION_USAGE_SOURCE"); err != nil {
		t.Fatalf("unset SCION_USAGE_SOURCE: %v", err)
	}

	var mu sync.Mutex
	var gotEvents []telemetrycontract.UsageEvent
	cfg := &Config{
		Enabled:              true,
		GRPCPort:             0,
		HTTPPort:             0,
		CloudEnabled:         true,
		CloudEnabledExplicit: true,
		CloudProvider:        "gcp",
		// Missing ProjectID with explicit CloudEnabled forces cloudInitDegraded=true
		// when Hub usage sink is configured.
	}
	p := NewWithConfig(cfg)
	p.SetAnalyticsOutbox(home, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			mu.Lock()
			gotEvents = append(gotEvents, req.Events...)
			mu.Unlock()
			return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
		},
		RunID:         "run-degraded-metering",
		HubID:         "hub-test",
		DrainInterval: 20 * time.Millisecond,
	})
	p.SetNativeMeteringOnly(true)

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	defer func() { _ = p.Stop(ctx) }()

	if !p.cloudInitDegraded {
		t.Fatal("precondition: expected cloudInitDegraded=true")
	}
	if !p.NativeMeteringOnly() {
		t.Fatal("precondition: expected NativeMeteringOnly()=true")
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Fatalf("DeliveryState() = %q, want degraded", got)
	}

	outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative)
	if err != nil {
		t.Fatalf("ActivateUsageSource: %v", err)
	}
	if outcome != UsageActivated {
		t.Fatalf("ActivateUsageSource outcome = %q, want %q", outcome, UsageActivated)
	}
	tailer := p.agyTailer.Load()
	if tailer == nil {
		t.Fatal("expected agyTailer to start under cloudInitDegraded + nativeMeteringOnly")
	}
	// Inject deterministic version detector so test does not depend on host agy binary.
	tailer.mu.Lock()
	tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
	tailer.mu.Unlock()

	// Insert a new conversation DB row after tailer startup/baselining.
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-degraded.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()
	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795712000, testAgyUsage{
		modelID: 1466, input: 777, output: 88, id11: "degraded-metering-call",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	if err := tailer.PollOnce(ctx); err != nil {
		t.Fatalf("tailer.PollOnce: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Pipeline.Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, ev := range gotEvents {
		if ev.UsageSource == telemetrycontract.UsageSourceSQLiteNative && ev.Input != nil && *ev.Input == 777 && ev.Output != nil && *ev.Output == 88 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected Hub to receive sqlite_native usage event (in=777 out=88) under cloudInitDegraded+nativeMeteringOnly, got %+v", gotEvents)
	}
}

func TestPipeline_Rev4_FinalHealthReportsSQLiteNativeEnabledAndPreservesDrift(t *testing.T) {
	cases := []struct {
		name  string
		drift bool
	}{
		{name: "healthy_stop", drift: false},
		{name: "drifted_stop", drift: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetTelemetryTestSandboxed())
			setTestAgyEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
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
			p.SetAnalyticsOutbox(home, 0, 0)
			p.SetHubUsageSink(HubUsageSinkConfig{
				Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
					return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
				},
				RunID: "run-final-health",
				HubID: "hub-test",
			})

			ctx := context.Background()
			if err := p.Start(ctx); err != nil {
				t.Fatalf("Pipeline.Start: %v", err)
			}
			p.SettleUsageSource(map[string]string{
				"SCION_USAGE_SOURCE": telemetrycontract.UsageSourceSQLiteNative,
			}, "")
			outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative)
			if err != nil {
				t.Fatalf("ActivateUsageSource: %v", err)
			}
			if outcome != UsageActivated {
				t.Fatalf("ActivateUsageSource = %q, want %q", outcome, UsageActivated)
			}
			tailer := p.agyTailer.Load()
			if tailer == nil {
				t.Fatal("expected active agyTailer")
			}
			if tc.drift {
				tailer.driftLatched.Store(true)
			}

			// Capture the final telemetry.health event produced during Stop.
			var mu sync.Mutex
			var finalEv *telemetrycontract.HealthEvent
			p.health.mu.Lock()
			p.health.enabled = true
			p.health.now = time.Now
			p.health.runID = "run-final-health"
			p.health.hubID = func(context.Context) string { return "hub-test" }
			p.health.send = func(_ context.Context, line []byte) error {
				var ev telemetrycontract.HealthEvent
				if err := json.Unmarshal(line, &ev); err != nil {
					return err
				}
				if ev.Trigger == telemetrycontract.HealthTriggerFinal {
					mu.Lock()
					evCopy := ev
					finalEv = &evCopy
					mu.Unlock()
				}
				return nil
			}
			p.health.mu.Unlock()

			if err := p.Stop(ctx); err != nil {
				t.Fatalf("Pipeline.Stop: %v", err)
			}

			mu.Lock()
			ev := finalEv
			mu.Unlock()
			if ev == nil {
				t.Fatal("expected final telemetry.health event on Pipeline.Stop")
			}
			if ev.UsageCollection != telemetrycontract.UsageCollectionEnabled {
				t.Fatalf("final health UsageCollection = %q (reason=%q), want %q", ev.UsageCollection, ev.UsageDisabledReason, telemetrycontract.UsageCollectionEnabled)
			}
			if ev.UsageSource != telemetrycontract.UsageSourceSQLiteNative {
				t.Fatalf("final health UsageSource = %q, want %q", ev.UsageSource, telemetrycontract.UsageSourceSQLiteNative)
			}
			if ev.UsageDisabledReason != "" {
				t.Fatalf("final health UsageDisabledReason = %q, want empty", ev.UsageDisabledReason)
			}
			if len(ev.UsageCapabilities) == 0 {
				t.Fatal("final health UsageCapabilities is empty, want sqlite_native capabilities")
			}
			if got := p.schemaDriftDetected(); got != tc.drift {
				t.Fatalf("schemaDriftDetected() after Stop = %v, want %v", got, tc.drift)
			}
		})
	}
}

func TestAntigravitySQLiteTailer_Rev4_BadDBDoesNotBlockBaselineForOtherDBs(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	convDir := AgyConversationsDir(home)
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}

	// Pre-populate one corrupt/unreadable DB and one valid DB with history.
	badPath := filepath.Join(convDir, "conv-a-bad.db")
	if err := os.WriteFile(badPath, []byte("not a valid sqlite database"), 0o600); err != nil {
		t.Fatalf("write bad db: %v", err)
	}
	goodPath := filepath.Join(convDir, "conv-b-good.db")
	goodDB := createTestAgyDB(t, goodPath)
	defer func() { _ = goodDB.Close() }()
	histGood := encodeTestGenMeta(1466, "gemini-4-neon", 1795700000, testAgyUsage{
		modelID: 1466, input: 9000, output: 1000, id11: "hist-good-0",
	})
	if _, err := goodDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, histGood, len(histGood)); err != nil {
		t.Fatalf("insert histGood: %v", err)
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

	// Poll 1: initial baseline returns error for bad DB, marks it baseline_pending,
	// baselines good DB, initializes state, and emits 0 events.
	if err := tailer.PollOnce(context.Background()); err == nil {
		t.Fatal("expected PollOnce to report error for bad DB during baseline")
	}
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("initial baseline emitted %d events, want 0", len(got))
	}

	// Insert a new row into the good DB: must emit immediately even while bad DB is still broken.
	newGood := encodeTestGenMeta(1466, "gemini-4-neon", 1795713000, testAgyUsage{
		modelID: 1466, input: 444, output: 55, id11: "new-good-1",
	})
	if _, err := goodDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, newGood, len(newGood)); err != nil {
		t.Fatalf("insert newGood: %v", err)
	}
	if err := tailer.PollOnce(context.Background()); err == nil {
		t.Fatal("expected PollOnce to still report error while bad DB remains broken")
	}
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 || events[0].Input == nil || *events[0].Input != 444 {
		t.Fatalf("good DB events = %+v, want 1 event with input=444", events)
	}

	// Replace bad DB with a valid DB containing historical row: retry baseline
	// must baseline it without emitting the historical row (never decode from rowid 0).
	if err := os.Remove(badPath); err != nil {
		t.Fatalf("remove bad db: %v", err)
	}
	repairedDB := createTestAgyDB(t, badPath)
	defer func() { _ = repairedDB.Close() }()
	histBad := encodeTestGenMeta(1466, "gemini-4-neon", 1795700500, testAgyUsage{
		modelID: 1466, input: 8000, output: 800, id11: "hist-bad-0",
	})
	if _, err := repairedDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, histBad, len(histBad)); err != nil {
		t.Fatalf("insert histBad: %v", err)
	}
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce after repairing bad DB: %v", err)
	}
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("repaired DB retry-baseline emitted %d historical events, want 0", len(got))
	}

	// Subsequent new row in the repaired DB must now emit normally.
	newRepaired := encodeTestGenMeta(1466, "gemini-4-neon", 1795713500, testAgyUsage{
		modelID: 1466, input: 222, output: 33, id11: "new-repaired-1",
	})
	if _, err := repairedDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, newRepaired, len(newRepaired)); err != nil {
		t.Fatalf("insert newRepaired: %v", err)
	}
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce after new row in repaired DB: %v", err)
	}
	repairedEvents := drainOutboxEvents(t, outbox)
	if len(repairedEvents) != 1 || repairedEvents[0].Input == nil || *repairedEvents[0].Input != 222 {
		t.Fatalf("repaired DB new events = %+v, want 1 event with input=222", repairedEvents)
	}
}
