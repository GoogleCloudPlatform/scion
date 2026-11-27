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

package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"google.golang.org/protobuf/encoding/protowire"
)

// backstopEnv sets up an agent home, a fake Hub and the environment that
// processHookData needs, as hook processes see it. It returns the home, the
// fake Hub, and a client factory for the init side.
func backstopEnv(t *testing.T) (string, *fakeMetricsHub, func() *hub.Client) {
	t.Helper()
	home := t.TempDir()
	scrubScionEnv(t)
	t.Setenv("HOME", home)
	log.SetLogPath(filepath.Join(home, "agent.log"))

	fake := &fakeMetricsHub{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "hook-test-agent")
	t.Setenv("SCION_TELEMETRY_ENABLED", "true")
	t.Setenv("SCION_TELEMETRY_CLOUD_ENABLED", "false")
	t.Setenv("SCION_OTEL_GRPC_PORT", strconv.Itoa(startDiscardingReceiver(t)))
	hookDialect = "claude"

	newClient := func() *hub.Client { return hub.NewClientWithConfig(srv.URL, "test-token", "hook-test-agent") }
	return home, fake, newClient
}

func runHooks(t *testing.T, events ...map[string]interface{}) {
	t.Helper()
	for _, ev := range events {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := processHookData(data); err != nil {
			t.Fatalf("processHookData(%v): %v", ev["hook_event_name"], err)
		}
	}
}

// openSessionEvents is a Claude session whose SessionEnd never arrives.
var openSessionEvents = []map[string]interface{}{
	{"hook_event_name": "SessionStart", "session_id": "sess-stop-1", "source": "startup"},
	{"hook_event_name": "PostToolUse", "session_id": "sess-stop-1", "tool_name": "Bash"},
	{"hook_event_name": "PostToolUse", "session_id": "sess-stop-1", "tool_name": "Bash"},
	{"hook_event_name": "Stop", "session_id": "sess-stop-1"},
}

var lateSessionEnd = map[string]interface{}{"hook_event_name": "SessionEnd", "session_id": "sess-stop-1", "reason": "other"}

// stopOutcome is the exit outcome of an agent stopped with SIGTERM, whose
// harness was killed by the signal.
func stopOutcome() exitOutcome { return classifyExit(-1, nil, nil, false, true, "") }

// The stop path: the harness is killed, so SessionEnd never runs. The init
// backstop reports the session once, with its ID and counts. A second check
// and a SessionEnd hook that arrives late add nothing.
func TestReportOpenSessionAtShutdown_StoppedAgentReportedOnce(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	runHooks(t, openSessionEvents...)
	if n := len(fake.Reports()); n != 0 {
		t.Fatalf("got %d reports before shutdown, want 0", n)
	}

	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)

	reports := fake.Reports()
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1: %+v", len(reports), reports)
	}
	p := reports[0]
	if p.Session.ID != "sess-stop-1" || p.Session.Status != "completed" || p.Session.TurnCount != 1 || p.Tools["Bash"].Calls != 2 {
		t.Errorf("payload = %+v", p)
	}
	if p.Session.StartedAt == "" || p.Session.EndedAt == "" {
		t.Errorf("session times missing: %+v", p.Session)
	}

	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)
	runHooks(t, lateSessionEnd)
	if n := len(fake.Reports()); n != 1 {
		t.Errorf("got %d reports after a second check and a late SessionEnd, want 1", n)
	}
}

// A session already reported by the SessionEnd hook is not reported again.
func TestReportOpenSessionAtShutdown_AlreadyReportedByHook(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	runHooks(t, append(append([]map[string]interface{}{}, openSessionEvents...), lateSessionEnd)...)
	if n := len(fake.Reports()); n != 1 {
		t.Fatalf("hook reported %d times, want 1", n)
	}
	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)
	if n := len(fake.Reports()); n != 1 {
		t.Errorf("got %d reports, want 1 (backstop must not repeat the hook's report)", n)
	}
}

// A crashed harness reports the session with status "error".
func TestReportOpenSessionAtShutdown_CrashStatus(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	runHooks(t, openSessionEvents...)
	code := 2
	reportOpenSessionAtShutdown(home, classifyExit(0, nil, &code, false, false, ""), newClient)
	reports := fake.Reports()
	if len(reports) != 1 || reports[0].Session.Status != "error" {
		t.Errorf("reports = %+v, want one with status error", reports)
	}
}

func TestReportOpenSessionAtShutdown_NothingToReport(t *testing.T) {
	t.Run("no state file", func(t *testing.T) {
		home, fake, newClient := backstopEnv(t)
		reportOpenSessionAtShutdown(home, stopOutcome(), newClient)
		if n := len(fake.Reports()); n != 0 {
			t.Errorf("got %d reports, want 0", n)
		}
	})
	t.Run("symlinked state file", func(t *testing.T) {
		home, fake, newClient := backstopEnv(t)
		// A real open session elsewhere, linked into the agent's home.
		other, _, _ := backstopEnv(t)
		t.Setenv("HOME", other)
		runHooks(t, openSessionEvents...)
		target := filepath.Join(other, ".scion", handlers.SessionStateFileName)
		want, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".scion", handlers.SessionStateFileName+".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(home, ".scion", handlers.SessionStateFileName)); err != nil {
			t.Fatal(err)
		}
		reportOpenSessionAtShutdown(home, stopOutcome(), newClient)
		if n := len(fake.Reports()); n != 0 {
			t.Errorf("got %d reports through a symlink, want 0", n)
		}
		if got, _ := os.ReadFile(target); string(got) != string(want) {
			t.Error("symlink target modified")
		}
	})
	t.Run("hub not configured", func(t *testing.T) {
		home, _, _ := backstopEnv(t)
		runHooks(t, openSessionEvents...)
		statePath := filepath.Join(home, ".scion", handlers.SessionStateFileName)
		before, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		reportOpenSessionAtShutdown(home, stopOutcome(), func() *hub.Client { return nil })
		reportOpenSessionAtShutdown(home, stopOutcome(), func() *hub.Client { return hub.NewClientWithConfig("", "", "") })
		if after, _ := os.ReadFile(statePath); string(after) != string(before) {
			t.Error("state changed without a Hub to report to")
		}
	})
}

// A Hub that does not answer cannot hold up shutdown past the bound; the
// report is dropped.
func TestReportOpenSessionAtShutdown_BoundedWhenHubHangs(t *testing.T) {
	home, _, _ := backstopEnv(t)
	runHooks(t, openSessionEvents...)

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer hung.Close()
	defer close(release)

	old := shutdownSessionReportTimeout
	shutdownSessionReportTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownSessionReportTimeout = old })

	start := time.Now()
	reportOpenSessionAtShutdown(home, stopOutcome(), func() *hub.Client {
		return hub.NewClientWithConfig(hung.URL, "test-token", "hook-test-agent")
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("backstop took %s with a hung Hub, want about %s", elapsed, shutdownSessionReportTimeout)
	}
}

// After a restart, init clears the previous shutdown's tombstone, so a
// resumed session that reuses the ID without a SessionStart is reported
// again at the next stop, with only the resumed segment's counts.
func TestReportOpenSessionAtShutdown_RestartResumeReportedAgain(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	segment := []map[string]interface{}{
		{"hook_event_name": "PostToolUse", "session_id": "sess-resume-1", "tool_name": "Bash"},
		{"hook_event_name": "Stop", "session_id": "sess-resume-1"},
	}
	runHooks(t, segment...)
	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)

	clearSessionTombstoneAtStartup(home)
	runHooks(t, segment...)
	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)

	reports := fake.Reports()
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2 (one per segment): %+v", len(reports), reports)
	}
	for i, p := range reports {
		if p.Session.ID != "sess-resume-1" || p.Session.TurnCount != 1 || p.Tools["Bash"].Calls != 1 {
			t.Errorf("report %d = %+v, want one segment's counts", i, p)
		}
	}
}

// Claude's hook events carry no usage, so with native usage the init
// daemon's sink adds it to the session state between the hook processes,
// and the SessionEnd hook's report includes it.
func TestSessionUsageRecorder_NativeUsageReachesSessionReport(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "native")
	record := sessionUsageRecorder(home)

	runHooks(t, openSessionEvents[:2]...)
	record(telemetry.SessionUsage{Calls: 1, TokensInput: 100, TokensOutput: 20, TokensCached: 300})
	runHooks(t, openSessionEvents[2:]...)
	record(telemetry.SessionUsage{Calls: 1, TokensInput: 50, TokensOutput: 5})
	runHooks(t, lateSessionEnd)

	// With native usage the session-end hook holds the report, so usage
	// the harness exports after session-end is added to it first.
	if n := len(fake.Reports()); n != 0 {
		t.Fatalf("got %d reports at session-end, want the report held", n)
	}
	record(telemetry.SessionUsage{Calls: 1, TokensInput: 7, TokensOutput: 3})

	// The harness exits; the init daemon's shutdown check sends the held
	// report at once, late usage included.
	reportOpenSessionAtShutdown(home, exitOutcome{}, newClient)
	reports := fake.Reports()
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1: %+v", len(reports), reports)
	}
	p := reports[0]
	if p.Session.ID != "sess-stop-1" || p.Tokens.Input != 157 || p.Tokens.Output != 28 || p.Tokens.Cached != 300 {
		t.Errorf("session %q tokens = %+v, want sess-stop-1 with input 157, output 28, cached 300", p.Session.ID, p.Tokens)
	}
	if p.Session.TurnCount != 1 || p.Tools["Bash"].Calls != 2 {
		t.Errorf("turns %d, tools %+v, want 1 turn and 2 Bash calls", p.Session.TurnCount, p.Tools)
	}

	// After the report the state is gone; later usage is dropped quietly:
	// it neither recreates the state nor causes another report.
	record(telemetry.SessionUsage{Calls: 1})
	if _, err := os.Stat(handlers.NewFileSessionState(home).Path); !os.IsNotExist(err) {
		t.Errorf("state file after the report: %v, want it absent", err)
	}
	reportOpenSessionAtShutdown(home, exitOutcome{}, newClient)
	if n := len(fake.Reports()); n != 1 {
		t.Errorf("got %d reports after late usage and a second check, want 1", n)
	}
	if sessionUsageRecorder("") != nil {
		t.Error("recorder without an agent home should be nil")
	}
}

// Under SCION_USAGE_SOURCE=sqlite_native, any harness that emits SessionEnd
// holds its session-end report while keeping hook-counted APICallCount from
// model-end hooks (defensive coverage; Antigravity itself has no SessionEnd
// hook and uses the shutdown flush + backstop path tested below).
func TestSessionUsageRecorder_SQLiteNativeHoldsReportAndKeepsHookCalls(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	hookDialect = "gemini"
	t.Setenv("SCION_USAGE_SOURCE", "sqlite_native")
	record := sessionUsageRecorder(home)

	runHooks(t,
		map[string]interface{}{"hook_event_name": "SessionStart", "session_id": "sess-agy-1"},
		map[string]interface{}{"hook_event_name": "AfterModel", "session_id": "sess-agy-1"},
		map[string]interface{}{"hook_event_name": "AfterModel", "session_id": "sess-agy-1"},
	)
	// Tailer emits tokens with Calls=0.
	record(telemetry.SessionUsage{TokensInput: 100, TokensOutput: 20, TokensCached: 300, TokensReasoning: 5})
	runHooks(t, map[string]interface{}{"hook_event_name": "SessionEnd", "session_id": "sess-agy-1"})

	// With sqlite_native, SessionEnd holds the report so debounced/final
	// SQLite tailer tokens are added first.
	if n := len(fake.Reports()); n != 0 {
		t.Fatalf("got %d reports at session-end, want the report held", n)
	}
	// Verify the held summary preserved hook-counted APICallCount from
	// model-end hooks (i.e. HookUsageFeedsSessionMetrics stayed true).
	rawState, err := os.ReadFile(handlers.NewFileSessionState(home).Path)
	if err != nil {
		t.Fatalf("read held session state: %v", err)
	}
	var heldState struct {
		Pending []struct {
			Summary telemetry.SessionSummary `json:"summary"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(rawState, &heldState); err != nil {
		t.Fatalf("unmarshal held session state: %v", err)
	}
	if len(heldState.Pending) != 1 || heldState.Pending[0].Summary.APICallCount != 2 {
		t.Fatalf("held pending = %+v, want 1 summary with APICallCount=2", heldState.Pending)
	}
	record(telemetry.SessionUsage{TokensInput: 40, TokensOutput: 10})

	reportOpenSessionAtShutdown(home, exitOutcome{}, newClient)
	reports := fake.Reports()
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1: %+v", len(reports), reports)
	}
	p := reports[0]
	if p.Session.ID != "sess-agy-1" {
		t.Errorf("session ID = %q, want sess-agy-1", p.Session.ID)
	}
	if p.Tokens.Input != 140 || p.Tokens.Output != 30 || p.Tokens.Cached != 300 || p.Tokens.Reasoning != 5 {
		t.Errorf("tokens = %+v, want input 140, output 30, cached 300, reasoning 5", p.Tokens)
	}
}

// TestAntigravitySQLiteNative_ShutdownFlushReachesBackstopReport exercises the
// real Antigravity production path: harnesses/antigravity/dialect.yaml maps
// PreInvocation/PostInvocation/Stop and has no SessionEnd event, so the
// session stays open until shutdown. Flushing the SQLite tailer before
// reportOpenSessionAtShutdown delivers final SQLite tokens to the session
// state before the backstop tombstones and reports it, while preserving
// hook-counted APICallCount and model name from PostInvocation.
func TestAntigravitySQLiteNative_ShutdownFlushReachesBackstopReport(t *testing.T) {
	home, fake, newClient := backstopEnv(t)

	// Install the real Antigravity dialect.yaml into $HOME/.scion/harness/.
	dialectSrc := filepath.Join("..", "..", "..", "harnesses", "antigravity", "dialect.yaml")
	dialectBytes, err := os.ReadFile(dialectSrc)
	if err != nil {
		t.Fatalf("read %s: %v", dialectSrc, err)
	}
	dialectDst := filepath.Join(home, ".scion", "harness", "dialect.yaml")
	if err := os.MkdirAll(filepath.Dir(dialectDst), 0o700); err != nil {
		t.Fatalf("mkdir harness dir: %v", err)
	}
	if err := os.WriteFile(dialectDst, dialectBytes, 0o600); err != nil {
		t.Fatalf("write dialect.yaml: %v", err)
	}

	// Put a stub agy binary on PATH reporting a verified version (1.5.18) so
	// AntigravitySQLiteTailer decodes gen_metadata rows without latching drift.
	binDir := t.TempDir()
	agyStub := filepath.Join(binDir, "agy")
	if err := os.WriteFile(agyStub, []byte("#!/bin/sh\necho 1.5.18\n"), 0o755); err != nil {
		t.Fatalf("write agy stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hookDialect = "antigravity"
	t.Setenv("SCION_HARNESS", "antigravity")
	t.Setenv("SCION_USAGE_SOURCE", "sqlite_native")
	t.Setenv("SCION_PROJECT_ID", "proj-agy-1")
	t.Setenv(telemetry.EnvUsageHubIngest, "1")

	ctx := context.Background()
	pipeline := telemetry.NewWithConfig(&telemetry.Config{
		Enabled:  true,
		GRPCPort: 0, // ephemeral loopback port
	})
	pipeline.SetAnalyticsOutbox(home, -1, -1)
	pipeline.SetHubUsageSink(telemetry.HubUsageSinkConfig{
		Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			return &telemetrycontract.UsageIngestResponse{}, nil
		},
		RunID:          "run-agy-1",
		HubID:          "hub-1",
		MaxBatchEvents: 100,
		MaxBatchBytes:  1 << 20,
		DrainInterval:  time.Hour,
	})
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer func() { _ = pipeline.Stop(ctx) }()
	wireSessionUsage(pipeline, home)

	// Open the session via real Antigravity hooks (no SessionEnd exists in
	// harnesses/antigravity/dialect.yaml).
	runHooks(t,
		map[string]interface{}{"hook_event_name": "PreInvocation", "conversationId": "sess-agy-shutdown-1", "modelName": "gemini-3.1-pro-low"},
		map[string]interface{}{"hook_event_name": "PostInvocation", "conversationId": "sess-agy-shutdown-1", "modelName": "gemini-3.1-pro-low"},
		map[string]interface{}{"hook_event_name": "PostInvocation", "conversationId": "sess-agy-shutdown-1", "modelName": "gemini-3.1-pro-low"},
		map[string]interface{}{"hook_event_name": "Stop", "conversationId": "sess-agy-shutdown-1", "terminationReason": "NO_TOOL_CALL"},
	)
	if n := len(fake.Reports()); n != 0 {
		t.Fatalf("got %d reports before shutdown, want 0 (Antigravity has no SessionEnd)", n)
	}

	// Write a final gen_metadata row to Antigravity's SQLite DB right before
	// shutdown (simulating the last turn flushed as agy exits, before the
	// periodic 15s poll fires).
	convDir := telemetry.AgyConversationsDir(home)
	if err := os.MkdirAll(convDir, 0o700); err != nil {
		t.Fatalf("mkdir conversations dir: %v", err)
	}
	dbPath := filepath.Join(convDir, "sess-agy-shutdown-1.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER, data BLOB, size INTEGER); CREATE TABLE steps (idx INTEGER, metadata BLOB);`); err != nil {
		_ = db.Close()
		t.Fatalf("create tables: %v", err)
	}
	var usage []byte
	usage = protowire.AppendTag(usage, 1, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 1466)
	usage = protowire.AppendTag(usage, 2, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 120)
	usage = protowire.AppendTag(usage, 3, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 35)
	usage = protowire.AppendTag(usage, 5, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 200)
	usage = protowire.AppendTag(usage, 9, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 7)
	usage = protowire.AppendTag(usage, 11, protowire.BytesType)
	usage = protowire.AppendString(usage, "agy-shutdown-row-1")
	var genMeta []byte
	genMeta = protowire.AppendTag(genMeta, 3, protowire.VarintType)
	genMeta = protowire.AppendVarint(genMeta, 1466)
	genMeta = protowire.AppendTag(genMeta, 4, protowire.BytesType)
	genMeta = protowire.AppendBytes(genMeta, usage)
	genMeta = protowire.AppendTag(genMeta, 5, protowire.BytesType)
	genMeta = protowire.AppendString(genMeta, "gemini-3.1-pro-low")
	var wrapper []byte
	wrapper = protowire.AppendTag(wrapper, 1, protowire.BytesType)
	wrapper = protowire.AppendBytes(wrapper, genMeta)
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, wrapper, len(wrapper)); err != nil {
		_ = db.Close()
		t.Fatalf("insert gen_metadata: %v", err)
	}
	_ = db.Close()

	// RunInit's shutdown sequence: flush SQLite tailer BEFORE the backstop
	// finalizes and tombstones the open session.
	flushSQLiteUsageBeforeBackstop(pipeline)

	// Confirm the state file holds both hook-counted APICallCount=2 and the
	// flushed SQLite tokens before the backstop sends and removes it.
	rawState, err := os.ReadFile(handlers.NewFileSessionState(home).Path)
	if err != nil {
		t.Fatalf("read session state after flush: %v", err)
	}
	var openState struct {
		Aggregator struct {
			APICallCount int `json:"api_call_count"`
		} `json:"aggregator"`
	}
	if err := json.Unmarshal(rawState, &openState); err != nil {
		t.Fatalf("unmarshal session state: %v", err)
	}
	if openState.Aggregator.APICallCount != 2 {
		t.Errorf("open session APICallCount = %d, want 2 (from PostInvocation hooks)", openState.Aggregator.APICallCount)
	}

	reportOpenSessionAtShutdown(home, exitOutcome{}, newClient)

	reports := fake.Reports()
	if len(reports) != 1 {
		t.Fatalf("got %d reports after shutdown backstop, want 1: %+v", len(reports), reports)
	}
	p := reports[0]
	if p.Session.ID != "sess-agy-shutdown-1" || p.Session.TurnCount != 1 {
		t.Errorf("session = %+v, want ID sess-agy-shutdown-1 and TurnCount 1", p.Session)
	}
	if p.Tokens.Input != 120 || p.Tokens.Output != 35 || p.Tokens.Cached != 200 || p.Tokens.Reasoning != 7 {
		t.Errorf("tokens = %+v, want input 120, output 35, cached 200, reasoning 7", p.Tokens)
	}
}

// Without native usage, session-end still sends at once.
func TestProcessHookData_NonNativeSessionEndSendsAtOnce(t *testing.T) {
	_, fake, _ := backstopEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	runHooks(t, openSessionEvents...)
	runHooks(t, lateSessionEnd)
	if reports := fake.Reports(); len(reports) != 1 || reports[0].Session.ID != "sess-stop-1" {
		t.Fatalf("reports = %+v, want the session reported at session-end", reports)
	}
}

// fakeSinkSetter captures the sink wireSessionUsage installs.
type fakeSinkSetter struct {
	sinks []telemetry.SessionUsageSink
}

func (f *fakeSinkSetter) SetSessionUsageSink(sink telemetry.SessionUsageSink) {
	f.sinks = append(f.sinks, sink)
}

// wireSessionUsage installs a sink that writes into the agent's session
// state, so usage it receives reaches the SessionEnd report. A nil pipeline
// is a no-op.
func TestWireSessionUsage_InstallsStateRecorder(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "native")

	setter := &fakeSinkSetter{}
	wireSessionUsage(setter, home)
	if len(setter.sinks) != 1 || setter.sinks[0] == nil {
		t.Fatalf("installed sinks = %d (nil: %v), want one non-nil", len(setter.sinks), len(setter.sinks) == 1 && setter.sinks[0] == nil)
	}

	runHooks(t, openSessionEvents...)
	setter.sinks[0](telemetry.SessionUsage{Calls: 1, TokensInput: 7, TokensOutput: 4})
	runHooks(t, lateSessionEnd)
	// Native usage: session-end holds the report; the shutdown check sends it.
	if n := len(fake.Reports()); n != 0 {
		t.Fatalf("got %d reports at session-end, want the report held", n)
	}
	reportOpenSessionAtShutdown(home, exitOutcome{}, newClient)
	reports := fake.Reports()
	if len(reports) != 1 || reports[0].Tokens.Input != 7 || reports[0].Tokens.Output != 4 {
		t.Errorf("reports = %+v, want one with input 7, output 4", reports)
	}

	wireSessionUsage(nil, home)
	wireSessionUsage((*telemetry.Pipeline)(nil), home)
}

// writeKilledHookReport leaves the agent's state file as a session-end hook
// process leaves it when it is killed after ending the session and before
// reporting it: the finalized summary is a pending report claimed by a
// process that no longer exists (a real child that has exited).
func writeKilledHookReport(t *testing.T, home, sessionID string) {
	t.Helper()
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Skipf("cannot run a child process: %v", err)
	}
	state := map[string]any{
		"version":    1,
		"aggregator": map[string]any{},
		"pending": []map[string]any{{
			"summary": map[string]any{
				"SessionID": sessionID,
				"StartedAt": time.Now().Add(-time.Minute).UTC(),
				"EndedAt":   time.Now().UTC(),
				"Status":    "completed",
				"TurnCount": 2,
				"ToolCalls": map[string]any{"Bash": map[string]int{"calls": 3, "success": 3}},
			},
			"claim_pid":  child.Process.Pid,
			"claimed_at": time.Now().UTC(),
		}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := handlers.NewFileSessionState(home).Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{path: data, path + ".lock": nil} {
		if err := os.WriteFile(name, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A session-end hook was killed between clearing the session and
// reporting it. The shutdown backstop sends that report once; a second
// check and later hooks send nothing more.
func TestReportOpenSessionAtShutdown_SendsReportOfKilledHook(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	writeKilledHookReport(t, home, "sess-killed-1")

	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)
	reports := fake.Reports()
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1: %+v", len(reports), reports)
	}
	if p := reports[0]; p.Session.ID != "sess-killed-1" || p.Session.TurnCount != 2 || p.Tools["Bash"].Calls != 3 {
		t.Errorf("payload = %+v", p)
	}

	reportOpenSessionAtShutdown(home, stopOutcome(), newClient)
	runHooks(t, openSessionEvents[0])
	if n := len(fake.Reports()); n != 1 {
		t.Errorf("got %d reports, want 1 (the pending report must be sent once)", n)
	}
}

// The same killed hook, with the next hook process picking the report up.
func TestProcessHookData_SendsReportOfKilledHook(t *testing.T) {
	home, fake, _ := backstopEnv(t)
	writeKilledHookReport(t, home, "sess-killed-2")

	runHooks(t, openSessionEvents...)
	reports := fake.Reports()
	if len(reports) != 1 || reports[0].Session.ID != "sess-killed-2" {
		t.Fatalf("reports = %+v, want the killed hook's report once", reports)
	}
}
