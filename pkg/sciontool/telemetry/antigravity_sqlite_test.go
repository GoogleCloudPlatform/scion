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
	"sync/atomic"
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

// collectExportedAgyTokens extracts the latest cumulative value per token_type
// for scion.usage.tokens (harness="antigravity") and counts any gen_ai.api.calls
// exported to backend.
func collectExportedAgyTokens(backend *captureOTLPBackend) (tokens map[string]int64, apiCalls int64) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	tokens = make(map[string]int64)
	for _, rm := range backend.metrics {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				switch m.GetName() {
				case telemetrycontract.MetricAPICalls:
					for _, dp := range m.GetSum().GetDataPoints() {
						apiCalls += dp.GetAsInt()
					}
				case telemetrycontract.MetricUsageTokens:
					for _, dp := range m.GetSum().GetDataPoints() {
						var harness, tokenType string
						for _, kv := range dp.GetAttributes() {
							switch kv.GetKey() {
							case telemetrycontract.HarnessLabel:
								harness = kv.GetValue().GetStringValue()
							case telemetrycontract.TokenTypeLabel:
								tokenType = kv.GetValue().GetStringValue()
							}
						}
						if harness == "antigravity" && tokenType != "" {
							if v := dp.GetAsInt(); v > tokens[tokenType] {
								tokens[tokenType] = v
							}
						}
					}
				}
			}
		}
	}
	return tokens, apiCalls
}

func TestPipeline_AgySQLiteTailer_SuppressDirectOTel(t *testing.T) {
	// (a) Unsuppressed Hub sink: OTLP backend gets the tailer's scion.usage.tokens
	// (no gen_ai.api.calls) and the Hub batch is unsuppressed.
	t.Run("unsuppressed_hub_sink", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		addr, backend := startCaptureOTLPBackend(t)
		var mu sync.Mutex
		var gotReqs []telemetrycontract.UsageIngestRequest
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
		})
		p.SetAnalyticsOutbox(home, 0, 0)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				mu.Lock()
				gotReqs = append(gotReqs, req)
				mu.Unlock()
				return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: false}, nil
			},
			RunID:              "run-agy-unsup",
			HubID:              "hub-test",
			SuppressDirectOTel: false,
			DrainInterval:      20 * time.Millisecond,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false")
		}
		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-unsup.db"))
		defer func() { _ = db.Close() }()
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730000, testAgyUsage{
			modelID: 1466, input: 500, output: 100, cacheRead: 1200, reasoning: 40, id11: "unsup-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		// Hub received unsuppressed sqlite_native event.
		mu.Lock()
		reqs := append([]telemetrycontract.UsageIngestRequest(nil), gotReqs...)
		mu.Unlock()
		var matchedReq *telemetrycontract.UsageIngestRequest
		var matchedEv *telemetrycontract.UsageEvent
		for i := range reqs {
			for j := range reqs[i].Events {
				if reqs[i].Events[j].UsageSource == telemetrycontract.UsageSourceSQLiteNative && reqs[i].Events[j].Input != nil && *reqs[i].Events[j].Input == 500 {
					matchedReq = &reqs[i]
					matchedEv = &reqs[i].Events[j]
					break
				}
			}
		}
		if matchedReq == nil || matchedEv == nil {
			t.Fatalf("expected Hub to receive sqlite_native event (in=500), got %+v", reqs)
		}
		if matchedReq.DirectOTelSuppressed || (matchedEv.DirectOTelSuppressed != nil && *matchedEv.DirectOTelSuppressed) {
			t.Fatalf("expected unsuppressed Hub batch and event, got req=%v ev=%v", matchedReq.DirectOTelSuppressed, matchedEv.DirectOTelSuppressed)
		}

		// OTLP backend received the tailer's exact token deltas directly (and 0 api calls).
		tokens, apiCalls := collectExportedAgyTokens(backend)
		if apiCalls != 0 {
			t.Fatalf("tailer must not emit gen_ai.api.calls, got %d", apiCalls)
		}
		if tokens[telemetrycontract.TokenTypeInput] != 500 ||
			tokens[telemetrycontract.TokenTypeOutput] != 100 ||
			tokens[telemetrycontract.TokenTypeCacheRead] != 1200 ||
			tokens[telemetrycontract.TokenTypeReasoning] != 40 {
			t.Fatalf("exported tailer tokens = %v, want input=500 output=100 cache_read=1200 reasoning=40", tokens)
		}
	})

	// (b) Suppressed Hub sink: OTLP backend gets NO tailer tokens, Hub batch is
	// suppressed, and heartbeats carry DirectOTelSuppressed=true.
	t.Run("suppressed_hub_sink", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		addr, backend := startCaptureOTLPBackend(t)
		var mu sync.Mutex
		var gotReqs []telemetrycontract.UsageIngestRequest
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
		})
		p.SetAnalyticsOutbox(home, 0, 0)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				mu.Lock()
				gotReqs = append(gotReqs, req)
				mu.Unlock()
				return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: true}, nil
			},
			RunID:              "run-agy-sup",
			HubID:              "hub-test",
			SuppressDirectOTel: true,
			DrainInterval:      20 * time.Millisecond,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true")
		}
		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-sup.db"))
		defer func() { _ = db.Close() }()
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730100, testAgyUsage{
			modelID: 1466, input: 500, output: 100, id11: "sup-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		mu.Lock()
		reqs := append([]telemetrycontract.UsageIngestRequest(nil), gotReqs...)
		mu.Unlock()
		var matchedReq *telemetrycontract.UsageIngestRequest
		var matchedEv *telemetrycontract.UsageEvent
		var sawSuppressedHeartbeat bool
		for i := range reqs {
			if len(reqs[i].Events) == 0 && reqs[i].DirectOTelSuppressed {
				sawSuppressedHeartbeat = true
			}
			for j := range reqs[i].Events {
				if reqs[i].Events[j].UsageSource == telemetrycontract.UsageSourceSQLiteNative && reqs[i].Events[j].Input != nil && *reqs[i].Events[j].Input == 500 {
					matchedReq = &reqs[i]
					matchedEv = &reqs[i].Events[j]
				}
			}
		}
		if matchedReq == nil || matchedEv == nil {
			t.Fatalf("expected Hub to receive sqlite_native event (in=500), got %+v", reqs)
		}
		if !matchedReq.DirectOTelSuppressed || matchedEv.DirectOTelSuppressed == nil || !*matchedEv.DirectOTelSuppressed {
			t.Fatalf("expected suppressed Hub batch and event, got req=%v ev=%v", matchedReq.DirectOTelSuppressed, matchedEv.DirectOTelSuppressed)
		}
		if !sawSuppressedHeartbeat {
			t.Fatalf("expected at least one heartbeat with DirectOTelSuppressed=true, got %+v", reqs)
		}

		tokens, apiCalls := collectExportedAgyTokens(backend)
		if len(tokens) != 0 || apiCalls != 0 {
			t.Fatalf("expected 0 direct OTLP tokens/calls while suppressed, got tokens=%v apiCalls=%d", tokens, apiCalls)
		}
	})

	// (c) GCP-only before late SetHubProbe activation: row 1 exports directly to
	// OTLP; after late Hub activation with suppress_direct_otel=true, row 2 is
	// stamped suppressed for Hub and NOT exported directly.
	t.Run("gcp_only_then_late_activation_with_suppression", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		addr, backend := startCaptureOTLPBackend(t)
		var mu sync.Mutex
		var gotReqs []telemetrycontract.UsageIngestRequest
		var probeReady atomic.Bool
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
		})
		p.SetAnalyticsOutbox(home, -1, -1)
		p.SetHubIDResolver(func(context.Context) (string, error) { return "hub-late", nil })
		p.SetHubUsageProbeInterval(5 * time.Millisecond)
		p.SetHubUsageProbe(func(context.Context) (*HubUsageSinkConfig, error) {
			if !probeReady.Load() {
				return nil, errors.New("hub not ready yet")
			}
			return &HubUsageSinkConfig{
				Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
					mu.Lock()
					gotReqs = append(gotReqs, req)
					mu.Unlock()
					return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: true}, nil
				},
				RunID:              "run-agy-late",
				HubID:              "hub-late",
				SuppressDirectOTel: true,
				DrainInterval:      20 * time.Millisecond,
			}, nil
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		// Enter GCP-only + hubProbe branch directly with a non-nil *GCPExporter stub.
		p.analytics.startDrainer(ctx, &GCPExporter{})
		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-late.db"))
		defer func() { _ = db.Close() }()

		// Row 1 while GCP-only (suppression off): must reach direct OTLP backend.
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730200, testAgyUsage{
			modelID: 1466, input: 300, output: 60, id11: "late-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 1: %v", err)
		}
		p.flushMetricsOnStop(ctx)
		tokensBefore, _ := collectExportedAgyTokens(backend)
		if tokensBefore[telemetrycontract.TokenTypeInput] != 300 || tokensBefore[telemetrycontract.TokenTypeOutput] != 60 {
			t.Fatalf("before late activation exported tokens = %v, want input=300 output=60", tokensBefore)
		}

		// Enable late Hub activation with suppression=true and drive one
		// deterministic drainer cycle (drainer's default ticker is 5s).
		p.analytics.drainer.Stop(ctx)
		probeReady.Store(true)
		time.Sleep(10 * time.Millisecond)
		p.analytics.drainer.RunOnce(ctx)
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true after late Hub activation")
		}
		// Restart drainer so row 2 drains cleanly on Pipeline.Stop.
		p.analytics.drainer.Start(ctx)

		// Row 2 while suppressed: must reach Hub stamped suppressed and NOT be exported directly.
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730205, testAgyUsage{
			modelID: 1466, input: 700, output: 140, id11: "late-call-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 2: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		mu.Lock()
		reqs := append([]telemetrycontract.UsageIngestRequest(nil), gotReqs...)
		mu.Unlock()
		var gotRow2Suppressed bool
		for _, r := range reqs {
			for _, ev := range r.Events {
				if ev.UsageSource == telemetrycontract.UsageSourceSQLiteNative && ev.Input != nil && *ev.Input == 700 &&
					ev.DirectOTelSuppressed != nil && *ev.DirectOTelSuppressed && r.DirectOTelSuppressed {
					gotRow2Suppressed = true
				}
			}
		}
		if !gotRow2Suppressed {
			t.Fatalf("expected Hub to receive row 2 (in=700) stamped suppressed, got %+v", reqs)
		}
		tokensAfter, _ := collectExportedAgyTokens(backend)
		if tokensAfter[telemetrycontract.TokenTypeInput] != 300 || tokensAfter[telemetrycontract.TokenTypeOutput] != 60 {
			t.Fatalf("after late suppression exported tokens = %v, want unchanged input=300 output=60", tokensAfter)
		}
	})

	// (d) Suppression revoked mid-run: row 1 is suppressed, Hub revokes (404),
	// then row 2 is exported directly to OTLP.
	t.Run("suppression_revoked_mid_run_then_direct_export", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		addr, backend := startCaptureOTLPBackend(t)
		var revoke atomic.Bool
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
		})
		p.SetAnalyticsOutbox(home, -1, -1)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				if revoke.Load() {
					return nil, ErrUsageIngestRevoked
				}
				return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: true}, nil
			},
			RunID:              "run-agy-rev",
			HubID:              "hub-rev",
			SuppressDirectOTel: true,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Millisecond,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true initially")
		}
		// Switch analytics drainer to eager dual-sink with GCP stub so revocation
		// keeps GCP analytics active while clearing Hub outbox and suppression.
		p.analytics.drainer.Stop(ctx)
		p.analytics.drainer = nil
		p.analytics.startDrainer(ctx, &GCPExporter{})
		p.analytics.drainer.Stop(ctx)

		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-rev.db"))
		defer func() { _ = db.Close() }()

		// Row 1 while suppressed: OTLP backend gets 0 tokens.
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730300, testAgyUsage{
			modelID: 1466, input: 400, output: 80, id11: "rev-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 1: %v", err)
		}
		p.flushMetricsOnStop(ctx)
		if tokensBefore, _ := collectExportedAgyTokens(backend); len(tokensBefore) != 0 {
			t.Fatalf("before revocation expected 0 direct tokens, got %v", tokensBefore)
		}

		// Revoke Hub sink (404) -> clears suppression.
		revoke.Store(true)
		time.Sleep(2 * time.Millisecond)
		p.analytics.drainer.RunOnce(ctx)
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false after Hub 404 revocation")
		}

		// Row 2 after revocation: must now export directly to OTLP backend.
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730305, testAgyUsage{
			modelID: 1466, input: 250, output: 50, id11: "rev-call-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 2: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		tokensAfter, apiCalls := collectExportedAgyTokens(backend)
		if apiCalls != 0 {
			t.Fatalf("tailer must not emit gen_ai.api.calls, got %d", apiCalls)
		}
		if tokensAfter[telemetrycontract.TokenTypeInput] < 250 || tokensAfter[telemetrycontract.TokenTypeOutput] < 50 {
			t.Fatalf("after revocation expected direct OTLP tokens for row 2 (>=250/50), got %v", tokensAfter)
		}
	})

	// (e) Crash between outbox append and state persist followed by replay must
	// NOT double-count tokens in OTel (AppendCertainDuplicate skips OTel Add).
	t.Run("crash_before_persist_replay_no_otel_double_count", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		addr, backend := startCaptureOTLPBackend(t)
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
		})
		p.SetAnalyticsOutbox(home, 0, 0)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: false}, nil
			},
			RunID:              "run-agy-replay-otel",
			HubID:              "hub-test",
			SuppressDirectOTel: false,
			DrainInterval:      20 * time.Millisecond,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-replay-otel.db"))
		defer func() { _ = db.Close() }()
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730400, testAgyUsage{
			modelID: 1466, input: 600, output: 120, id11: "replay-otel-1",
		})
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730405, testAgyUsage{
			modelID: 1466, input: 400, output: 80, id11: "replay-otel-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?), (1, ?, ?)`, gen0, len(gen0), gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen0/gen1: %v", err)
		}

		// Fail 2nd append on poll 1 so record 0 is appended + OTel-recorded while
		// state is NOT persisted (simulating crash/partial failure before persist).
		callNum := 0
		p.analytics.sqliteNative.setRunAndActivity("run-agy-replay-otel", func(time.Time) {
			callNum++
			if callNum == 2 {
				_ = os.Setenv("SCION_AGENT_ID", "")
			}
		})
		if err := tailer.PollOnce(ctx); err == nil {
			t.Fatal("expected PollOnce to fail on 2nd record append")
		}

		// Restore agent identity and replay: record 0 hits AppendCertainDuplicate
		// (must NOT re-add to OTel), record 1 hits AppendWritten (added once).
		t.Setenv("SCION_AGENT_ID", "agent-agy-test")
		p.analytics.sqliteNative.setRunAndActivity("run-agy-replay-otel", nil)
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("retry PollOnce: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		tokens, apiCalls := collectExportedAgyTokens(backend)
		if apiCalls != 0 {
			t.Fatalf("tailer must not emit gen_ai.api.calls, got %d", apiCalls)
		}
		if tokens[telemetrycontract.TokenTypeInput] != 1000 || tokens[telemetrycontract.TokenTypeOutput] != 200 {
			t.Fatalf("exported tokens after replay = %v, want exact input=1000 output=200 (no double-count)", tokens)
		}
	})
}

func TestAntigravitySQLiteTailer_RevocationCursorCommit(t *testing.T) {
	// Pure Hub-only mode (no cloud/OTLP exporter): after a Hub 404 revocation
	// detaches sqliteNative, PollOnce must NOT advance cursors, mutate Dedupe,
	// or accumulate session usage; rows remain unconsumed so a later active sink
	// can still report them.
	t.Run("hub_only_revocation_withholds_cursor_and_side_effects", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		var revoke atomic.Bool
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: false,
		})
		p.SetAnalyticsOutbox(home, -1, -1)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				if revoke.Load() {
					return nil, ErrUsageIngestRevoked
				}
				return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: true}, nil
			},
			RunID:              "run-agy-hubonly-rev",
			HubID:              "hub-rev",
			SuppressDirectOTel: true,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Millisecond,
		})

		var sessionMu sync.Mutex
		var sessionTotal SessionUsage
		p.SetSessionUsageSink(func(u SessionUsage) {
			sessionMu.Lock()
			sessionTotal.TokensInput += u.TokensInput
			sessionTotal.TokensOutput += u.TokensOutput
			sessionMu.Unlock()
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		defer func() { _ = p.Stop(ctx) }()
		// Stop the background drainer loop so this test drives RunOnce synchronously.
		p.analytics.drainer.Stop(ctx)

		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-hubonly-rev.db"))
		defer func() { _ = db.Close() }()

		// Row 0 before revocation: accepted by Hub outbox and advances cursor.
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730500, testAgyUsage{
			modelID: 1466, input: 400, output: 80, id11: "hubonly-rev-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 0: %v", err)
		}

		tailer.mu.Lock()
		cursorAfterRow0 := tailer.state.Files["conv-hubonly-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()
		if cursorAfterRow0.GenMetadataRowID == nil {
			t.Fatalf("expected row 0 to advance cursor, got %+v", cursorAfterRow0)
		}

		// Revoke Hub sink (404) -> detaches sqliteNative in Hub-only mode.
		revoke.Store(true)
		time.Sleep(2 * time.Millisecond)
		p.analytics.drainer.RunOnce(ctx)
		if !p.analytics.sqliteNative.IsDisabled() {
			t.Fatal("expected sqliteNative.IsDisabled()=true after pure Hub-only 404 revocation")
		}

		// Row 1 after revocation: must NOT advance cursor or accumulate session usage.
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730505, testAgyUsage{
			modelID: 1466, input: 250, output: 50, id11: "hubonly-rev-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 1 after revocation: %v", err)
		}

		tailer.mu.Lock()
		cursorAfterRevoked := tailer.state.Files["conv-hubonly-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()
		if cursorAfterRevoked.GenMetadataRowID == nil || *cursorAfterRevoked.GenMetadataRowID != *cursorAfterRow0.GenMetadataRowID {
			t.Fatalf("cursor advanced after pure Hub-only revocation: got %d, want %d",
				*cursorAfterRevoked.GenMetadataRowID, *cursorAfterRow0.GenMetadataRowID)
		}

		sessionMu.Lock()
		gotSession := sessionTotal
		sessionMu.Unlock()
		if gotSession.TokensInput != 400 || gotSession.TokensOutput != 80 {
			t.Fatalf("session usage after revocation = %+v, want only row 0 (400/80)", gotSession)
		}

		// Verify row 1 is still unconsumed: a new tailer pointing at an active
		// outbox and loading the same persisted state file emits row 1.
		recoveryOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "recovery-ob"))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox: %v", err)
		}
		recoveryTailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         NewUsageAnalytics(recoveryOutbox, telemetrycontract.UsageSourceSQLiteNative),
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		})
		if err := recoveryTailer.PollOnce(ctx); err != nil {
			t.Fatalf("recovery PollOnce: %v", err)
		}
		evs := stagedEvents(t, recoveryOutbox)
		if len(evs) != 1 || evs[0].Input == nil || *evs[0].Input != 250 {
			t.Fatalf("recovery outbox events = %+v, want 1 event for row 1 (input=250)", evs)
		}
	})

	// Hub + generic OTLP exporter (Option A): Hub 404 detaches sqliteNative and
	// unsuppresses direct OTel export; PollOnce must record row 1 to the OTLP
	// exporter and advance cursors so row 1 is exported once.
	t.Run("hub_plus_otlp_revocation_records_otel_and_advances_cursor", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		addr, backend := startCaptureOTLPBackend(t)
		var revoke atomic.Bool
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
		})
		p.SetAnalyticsOutbox(home, -1, -1)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				if revoke.Load() {
					return nil, ErrUsageIngestRevoked
				}
				return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events), SuppressDirectOTel: true}, nil
			},
			RunID:              "run-agy-hub-otlp-rev",
			HubID:              "hub-rev",
			SuppressDirectOTel: true,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Millisecond,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		// Stop the background drainer loop so this test drives RunOnce synchronously.
		p.analytics.drainer.Stop(ctx)
		if outcome, err := p.ActivateUsageSource(ctx, telemetrycontract.UsageSourceSQLiteNative); err != nil || outcome != UsageActivated {
			t.Fatalf("ActivateUsageSource = (%q, %v), want (%q, nil)", outcome, err, UsageActivated)
		}
		tailer := p.agyTailer.Load()
		tailer.mu.Lock()
		tailer.cfg.DetectVersion = func(context.Context) (string, error) { return "1.5.18", nil }
		tailer.mu.Unlock()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-hub-otlp-rev.db"))
		defer func() { _ = db.Close() }()

		// Row 0 while suppressed: OTLP backend gets 0 tokens.
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730600, testAgyUsage{
			modelID: 1466, input: 400, output: 80, id11: "hub-otlp-rev-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 0: %v", err)
		}
		p.flushMetricsOnStop(ctx)
		if tokensBefore, _ := collectExportedAgyTokens(backend); len(tokensBefore) != 0 {
			t.Fatalf("before revocation expected 0 direct tokens, got %v", tokensBefore)
		}

		// Revoke Hub sink (404) -> detaches sqliteNative and unsuppresses direct OTel.
		revoke.Store(true)
		time.Sleep(2 * time.Millisecond)
		p.analytics.drainer.RunOnce(ctx)
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false after Hub 404 revocation")
		}

		tailer.mu.Lock()
		cursorAfterRow0 := tailer.state.Files["conv-hub-otlp-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()

		// Row 1 after revocation: exported via OTLP and cursor advances.
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730605, testAgyUsage{
			modelID: 1466, input: 250, output: 50, id11: "hub-otlp-rev-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 1: %v", err)
		}

		tailer.mu.Lock()
		cursorAfterRow1 := tailer.state.Files["conv-hub-otlp-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()
		if cursorAfterRow0.GenMetadataRowID == nil || cursorAfterRow1.GenMetadataRowID == nil ||
			*cursorAfterRow1.GenMetadataRowID <= *cursorAfterRow0.GenMetadataRowID {
			t.Fatalf("expected cursor to advance in Hub+OTLP mode after revocation: got %v <= %v",
				cursorAfterRow1.GenMetadataRowID, cursorAfterRow0.GenMetadataRowID)
		}

		if err := p.Stop(ctx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		tokensAfter, apiCalls := collectExportedAgyTokens(backend)
		if apiCalls != 0 {
			t.Fatalf("tailer must not emit gen_ai.api.calls, got %d", apiCalls)
		}
		if tokensAfter[telemetrycontract.TokenTypeInput] != 250 || tokensAfter[telemetrycontract.TokenTypeOutput] != 50 {
			t.Fatalf("after Hub+OTLP revocation expected exact direct OTLP tokens 250/50, got %v", tokensAfter)
		}
	})

	// Dual-run (GCP + Hub) revocation clears Hub outbox while GCP outbox remains
	// active: PollOnce must continue appending to GCP outbox and advancing cursors.
	t.Run("dual_run_gcp_hub_revocation_continues_advancing", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		gcpOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "gcp-ob"))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox gcp: %v", err)
		}
		hubOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub-ob"))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox hub: %v", err)
		}
		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceSQLiteNative)

		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         dual,
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		})

		ctx := context.Background()
		if err := tailer.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = tailer.Stop(ctx) }()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-dual-rev.db"))
		defer func() { _ = db.Close() }()

		// Simulate dual-run Hub 404 revocation: clears Hub outbox, keeps GCP outbox.
		dual.clearHubOutbox()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730700, testAgyUsage{
			modelID: 1466, input: 300, output: 60, id11: "dual-rev-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}

		tailer.mu.Lock()
		cur := tailer.state.Files["conv-dual-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()
		if cur.GenMetadataRowID == nil {
			t.Fatalf("expected cursor to advance in dual-run after Hub clear, got %+v", cur)
		}
		if gcpEvs := stagedEvents(t, gcpOutbox); len(gcpEvs) != 1 || gcpEvs[0].Input == nil || *gcpEvs[0].Input != 300 {
			t.Fatalf("gcp outbox events = %+v, want 1 event (input=300)", gcpEvs)
		}
		if hubEvs := stagedEvents(t, hubOutbox); len(hubEvs) != 0 {
			t.Fatalf("hub outbox events = %d, want 0 after clearHubOutbox", len(hubEvs))
		}
	})

	// When RecordUnique returns AppendDisabled mid-poll (e.g. inactive Hub-only
	// producer or concurrent detach) with no active OTel exporter, cursors and
	// session usage must not be committed.
	t.Run("mid_poll_append_disabled_withholds_cursor", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)

		// Inactive hubOnly producer: IsDisabled() is false, but RecordUnique
		// returns (AppendDisabled, nil) inside the record loop.
		inactive := NewLazyHookUsageAnalyticsForSource(home, false, nil, telemetrycontract.UsageSourceSQLiteNative)
		var sessionCalls int
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         inactive,
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
			SessionUsage: func(SessionUsage) {
				sessionCalls++
			},
		})
		ctx := context.Background()
		if err := tailer.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = tailer.Stop(ctx) }()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-inactive.db"))
		defer func() { _ = db.Close() }()
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730800, testAgyUsage{
			modelID: 1466, input: 150, output: 30, id11: "inactive-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		tailer.mu.Lock()
		_, hasFile := tailer.state.Files["conv-inactive.db"]
		tailer.mu.Unlock()
		if hasFile {
			t.Fatalf("expected cursor not to be committed when RecordUnique returns AppendDisabled")
		}
		if sessionCalls != 0 {
			t.Fatalf("expected 0 SessionUsage calls on AppendDisabled, got %d", sessionCalls)
		}
	})

	// Optional 1: when schema drift is already latched and Hub-only revocation
	// detaches Analytics, PollOnce must still run the drift-latched BaselineFile
	// path so rows written during a drifted+revoked run advance the cursor and
	// are not retroactively billed by a later run, while emitting no events.
	t.Run("drift_latched_hub_only_revocation_still_rebaselines", func(t *testing.T) {
		t.Cleanup(SetTelemetryTestSandboxed())
		setTestAgyEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_USAGE_SOURCE", "")
		_ = os.Unsetenv("SCION_USAGE_SOURCE")

		hubOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub-ob"))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox: %v", err)
		}
		analytics := NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative)
		var sessionCalls int
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         analytics,
			// Unverified agy version triggers schema drift on the first decoded row.
			DetectVersion: func(context.Context) (string, error) { return "99.0.0-unverified", nil },
			SessionUsage: func(SessionUsage) {
				sessionCalls++
			},
		})
		ctx := context.Background()
		if err := tailer.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = tailer.Stop(ctx) }()

		db := createTestAgyDB(t, filepath.Join(AgyConversationsDir(home), "conv-drift-rev.db"))
		defer func() { _ = db.Close() }()

		// Row 0 latches schema drift (emits 1 drift sentinel event).
		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730900, testAgyUsage{
			modelID: 1466, input: 100, output: 20, id11: "drift-rev-0",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 0: %v", err)
		}
		if !tailer.SchemaDriftDetected() {
			t.Fatal("expected schema drift to be latched after unverified version")
		}
		tailer.mu.Lock()
		cursorAfterDrift := tailer.state.Files["conv-drift-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()
		if cursorAfterDrift.GenMetadataRowID == nil {
			t.Fatalf("expected cursor after drift latch, got %+v", cursorAfterDrift)
		}
		eventsBeforeRevoke := len(stagedEvents(t, hubOutbox))

		// Detach Analytics (pure Hub-only 404 revocation; no OTel exporter).
		analytics.detach()

		// Row 1 written during the drifted+revoked run must advance the cursor
		// via BaselineFile without emitting any analytics event or session usage.
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795730905, testAgyUsage{
			modelID: 1466, input: 200, output: 40, id11: "drift-rev-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		if err := tailer.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce row 1 after drift+revocation: %v", err)
		}

		tailer.mu.Lock()
		cursorAfterRow1 := tailer.state.Files["conv-drift-rev.db"].Cursor.Clone()
		tailer.mu.Unlock()
		if cursorAfterRow1.GenMetadataRowID == nil || *cursorAfterRow1.GenMetadataRowID <= *cursorAfterDrift.GenMetadataRowID {
			t.Fatalf("expected drift-latched re-baseline to advance cursor after Hub-only revocation: got %+v, want > %d",
				cursorAfterRow1, *cursorAfterDrift.GenMetadataRowID)
		}
		if got := len(stagedEvents(t, hubOutbox)); got != eventsBeforeRevoke {
			t.Fatalf("outbox events changed after drift+revocation: got %d, want %d", got, eventsBeforeRevoke)
		}
		if sessionCalls != 0 {
			t.Fatalf("expected 0 SessionUsage calls after drift+revocation, got %d", sessionCalls)
		}
	})
}

func TestAntigravitySQLiteTailer_StopBoundsFinalPollTimeout(t *testing.T) {
	setTestAgyEnv(t)
	origTimeout := usageDeriverFlushTimeout
	usageDeriverFlushTimeout = 50 * time.Millisecond
	t.Cleanup(func() { usageDeriverFlushTimeout = origTimeout })

	home := t.TempDir()
	seedEmptyAgyState(t, home)
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-stop-bound.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795740000, testAgyUsage{
		modelID: 1466, input: 100, output: 20, id11: "stop-bound-1",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}

	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		PollInterval:      time.Hour,
		DetectVersion: func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	if err := tailer.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	parentCtx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	stopErr := tailer.Stop(parentCtx)
	elapsed := time.Since(start)

	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("Stop err = %v, want context.DeadlineExceeded", stopErr)
	}
	if parentCtx.Err() != nil {
		t.Fatalf("Stop consumed the entire parent shutdown deadline (elapsed=%s, parentCtx.Err=%v)", elapsed, parentCtx.Err())
	}
}

func TestAntigravitySQLiteTailer_OutboxRetryBackoff(t *testing.T) {
	t.Run("loop_suppresses_retries_during_overflow_backoff", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		seedEmptyAgyState(t, home)
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-backoff.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795741000, testAgyUsage{
			modelID: 1466, input: 300, output: 60, id11: "backoff-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open hub outbox: %v", err)
		}
		hubOutbox.maxPending = 128

		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
			Debounce:          10 * time.Millisecond,
			PollInterval:      25 * time.Millisecond,
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		})
		if err := tailer.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		// Trigger repeated wakes and let several periodic ticks fire while overflowed.
		for range 4 {
			tailer.TriggerWake()
			time.Sleep(35 * time.Millisecond)
		}

		overflowsDuringBackoff := hubOutbox.Stats().Overflow
		if overflowsDuringBackoff != 1 {
			_ = tailer.Stop(context.Background())
			t.Fatalf("outbox overflow count during backoff = %d, want 1 (repeated retries burned producer_seq)", overflowsDuringBackoff)
		}

		// Free space and verify Stop's final poll bypasses backoff and delivers the row.
		hubOutbox.maxPending = analyticsOutboxMaxPending
		if err := tailer.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		events := drainOutboxEvents(t, hubOutbox)
		if len(events) != 1 {
			t.Fatalf("got %d events after Stop drain, want 1", len(events))
		}
		if events[0].ProducerSeq != 2 {
			t.Fatalf("ProducerSeq = %d, want 2 (1 refused overflow + 1 delivered event, no seq gap)", events[0].ProducerSeq)
		}
	})

	t.Run("exponential_backoff_cap_and_reset_on_success", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		seedEmptyAgyState(t, home)
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-exp-backoff.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795741100, testAgyUsage{
			modelID: 1466, input: 400, output: 80, id11: "exp-backoff-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open hub outbox: %v", err)
		}
		hubOutbox.maxPending = 128

		now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:             home,
			DisableWakeSocket:     true,
			Analytics:             NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
			OutboxRetryMinBackoff: 5 * time.Second,
			OutboxRetryMaxBackoff: 15 * time.Second,
			DetectVersion:         func(context.Context) (string, error) { return "1.5.18", nil },
			Now:                   func() time.Time { return now },
		})

		// Attempt 1 at t=0: overflows -> backoff 5s (retryAfter = t+5s).
		if err := tailer.pollOnce(context.Background(), true); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("attempt 1 err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 1 {
			t.Fatalf("after attempt 1 overflow = %d, want 1", got)
		}

		// At t+4s: still within 5s backoff -> suppressed (no error, no overflow).
		now = now.Add(4 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("suppressed poll at t+4s err = %v, want nil", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 1 {
			t.Fatalf("at t+4s overflow = %d, want 1", got)
		}

		// At t+5s: retries -> overflows again -> backoff doubles to 10s (retryAfter = t+15s).
		now = now.Add(1 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("attempt 2 err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 2 {
			t.Fatalf("after attempt 2 overflow = %d, want 2", got)
		}

		// At t+14s: still within 10s backoff -> suppressed.
		now = now.Add(9 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("suppressed poll at t+14s err = %v, want nil", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 2 {
			t.Fatalf("at t+14s overflow = %d, want 2", got)
		}

		// At t+15s: retries -> overflows again -> backoff capped at 15s (retryAfter = t+30s).
		now = now.Add(1 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("attempt 3 err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 3 {
			t.Fatalf("after attempt 3 overflow = %d, want 3", got)
		}
		now = now.Add(14 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("suppressed poll at t+29s err = %v, want nil", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 3 {
			t.Fatalf("at t+29s overflow = %d, want 3", got)
		}

		// Free space and advance to t+30s: succeeds and resets backoff immediately.
		hubOutbox.maxPending = analyticsOutboxMaxPending
		now = now.Add(1 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("recovery poll err = %v, want nil", err)
		}
		if tailer.outboxBackoff != 0 || !tailer.outboxRetryAfter.IsZero() {
			t.Fatalf("backoff not reset after success: backoff=%s retryAfter=%v", tailer.outboxBackoff, tailer.outboxRetryAfter)
		}

		// Next overflow must start fresh at minBackoff (5s), not capped 15s.
		hubOutbox.maxPending = 128
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795741200, testAgyUsage{
			modelID: 1466, input: 100, output: 20, id11: "exp-backoff-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}
		if err := tailer.pollOnce(context.Background(), true); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("post-reset overflow err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if tailer.outboxBackoff != 5*time.Second {
			t.Fatalf("post-reset backoff = %s, want 5s", tailer.outboxBackoff)
		}
	})

	t.Run("relocated_refused_triggers_backoff", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		seedEmptyAgyState(t, home)
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-reloc.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795741300, testAgyUsage{
			modelID: 1466, input: 200, output: 40, id11: "reloc-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open hub outbox: %v", err)
		}
		if _, err := hubOutbox.PinHubID("hub-A"); err != nil {
			t.Fatalf("PinHubID hub-A: %v", err)
		}
		if err := hubOutbox.recordRelocation("hub-B"); err != nil {
			t.Fatalf("recordRelocation hub-B: %v", err)
		}

		now := time.Date(2026, 11, 27, 16, 10, 0, 0, time.UTC)
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:             home,
			DisableWakeSocket:     true,
			Analytics:             NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
			OutboxRetryMinBackoff: 5 * time.Second,
			DetectVersion:         func(context.Context) (string, error) { return "1.5.18", nil },
			Now:                   func() time.Time { return now },
		})

		if err := tailer.pollOnce(context.Background(), true); !errors.Is(err, ErrAnalyticsOutboxRelocatedRefused) {
			t.Fatalf("poll 1 err = %v, want ErrAnalyticsOutboxRelocatedRefused", err)
		}
		if got := hubOutbox.Stats().RelocatedRefused; got != 1 {
			t.Fatalf("RelocatedRefused = %d, want 1", got)
		}

		// Retry within backoff is suppressed and does not consume another producer_seq.
		now = now.Add(3 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("suppressed poll err = %v, want nil", err)
		}
		if got := hubOutbox.Stats().RelocatedRefused; got != 1 {
			t.Fatalf("RelocatedRefused during backoff = %d, want 1", got)
		}
	})
}

func TestAntigravitySQLiteTailer_DriftSentinelRetry(t *testing.T) {
	t.Run("unverified_version_retries_after_outbox_overflow", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		seedEmptyAgyState(t, home)
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-drift-retry-unv.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795742000, testAgyUsage{
			modelID: 1466, input: 300, output: 60, id11: "drift-retry-unv-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open hub outbox: %v", err)
		}
		hubOutbox.maxPending = 128

		now := time.Date(2026, 11, 27, 16, 20, 0, 0, time.UTC)
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:             home,
			DisableWakeSocket:     true,
			Analytics:             NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
			OutboxRetryMinBackoff: 5 * time.Second,
			DetectVersion:         func(context.Context) (string, error) { return "99.0.0-unverified", nil },
			Now:                   func() time.Time { return now },
		})

		// Poll 1: drift latches immediately, but sentinel append overflows.
		if err := tailer.PollOnce(context.Background()); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("poll 1 err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if !tailer.SchemaDriftDetected() {
			t.Fatal("expected SchemaDriftDetected() == true immediately after drift")
		}
		if got := drainOutboxEvents(t, hubOutbox); len(got) != 0 {
			t.Fatalf("expected 0 events after overflow, got %d", len(got))
		}

		// Background poll during backoff is suppressed and does not burn another producer_seq.
		now = now.Add(2 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("suppressed poll err = %v, want nil", err)
		}
		if got := hubOutbox.Stats().Overflow; got != 1 {
			t.Fatalf("overflow count during backoff = %d, want 1", got)
		}

		// Free outbox space and advance past backoff -> sentinel is retried and emitted once.
		hubOutbox.maxPending = analyticsOutboxMaxPending
		now = now.Add(4 * time.Second)
		if err := tailer.pollOnce(context.Background(), true); err != nil {
			t.Fatalf("poll 2 (retry) err = %v, want nil", err)
		}
		// Poll 3: must not emit a duplicate sentinel.
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 3 err = %v, want nil", err)
		}

		events := drainOutboxEvents(t, hubOutbox)
		if len(events) != 1 {
			t.Fatalf("got %d events, want exactly 1 retried unverified drift sentinel", len(events))
		}
		if events[0].TokenStatus != telemetrycontract.TokenStatusUnverified || events[0].Calls != 0 || events[0].Input != nil {
			t.Fatalf("unexpected drift sentinel event: %+v", events[0])
		}
	})

	t.Run("malformed_decode_retries_after_outbox_overflow", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		seedEmptyAgyState(t, home)
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-drift-retry-mal.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, 3)`, []byte{0xff, 0xff, 0xff}); err != nil {
			t.Fatalf("insert corrupt gen0: %v", err)
		}

		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open hub outbox: %v", err)
		}
		hubOutbox.maxPending = 128

		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
			DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
		})

		if err := tailer.PollOnce(context.Background()); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("poll 1 err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if !tailer.SchemaDriftDetected() {
			t.Fatal("expected SchemaDriftDetected() == true immediately after malformed decode")
		}

		hubOutbox.maxPending = analyticsOutboxMaxPending
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 2 (retry) err = %v, want nil", err)
		}
		events := drainOutboxEvents(t, hubOutbox)
		if len(events) != 1 || events[0].TokenStatus != telemetrycontract.TokenStatusMalformed {
			t.Fatalf("got events %+v, want 1 malformed drift sentinel", events)
		}
	})

	t.Run("version_detect_drift_retries_after_outbox_overflow", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		seedEmptyAgyState(t, home)
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-drift-retry-vfail.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795742100, testAgyUsage{
			modelID: 1466, input: 150, output: 30, id11: "drift-retry-vfail-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open hub outbox: %v", err)
		}
		hubOutbox.maxPending = 128

		now := time.Date(2026, 11, 27, 16, 30, 0, 0, time.UTC)
		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:               home,
			DisableWakeSocket:       true,
			Analytics:               NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
			MaxVersionFailures:      2,
			MinVersionFailureWindow: 1 * time.Minute,
			DetectVersion:           func(context.Context) (string, error) { return "", context.DeadlineExceeded },
			Now:                     func() time.Time { return now },
		})

		// Poll 1: failure 1 of 2.
		if err := tailer.PollOnce(context.Background()); err == nil {
			t.Fatal("poll 1: expected version detection error")
		}
		if tailer.SchemaDriftDetected() {
			t.Fatal("poll 1: must not latch before failure window")
		}

		// Poll 2 at t+2m: latches version-detect drift, but sentinel append overflows.
		now = now.Add(2 * time.Minute)
		if err := tailer.PollOnce(context.Background()); !errors.Is(err, ErrAnalyticsOutboxOverflow) {
			t.Fatalf("poll 2 err = %v, want ErrAnalyticsOutboxOverflow", err)
		}
		if !tailer.SchemaDriftDetected() {
			t.Fatal("poll 2: expected SchemaDriftDetected() == true")
		}
		if got := drainOutboxEvents(t, hubOutbox); len(got) != 0 {
			t.Fatalf("expected 0 events after overflow, got %d", len(got))
		}

		// Poll 3 after freeing space: pending sentinel is retried and emitted once.
		hubOutbox.maxPending = analyticsOutboxMaxPending
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 3 err = %v, want nil", err)
		}
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 4 err = %v, want nil", err)
		}
		events := drainOutboxEvents(t, hubOutbox)
		if len(events) != 1 || events[0].TokenStatus != telemetrycontract.TokenStatusUnverified || events[0].Calls != 0 || events[0].Input != nil {
			t.Fatalf("got events %+v, want 1 unverified version-detect drift sentinel", events)
		}
	})
}
