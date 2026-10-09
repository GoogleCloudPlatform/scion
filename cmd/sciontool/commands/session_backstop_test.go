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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
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
func stopOutcome() exitOutcome { return classifyExit(-1, nil, nil, false, true) }

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
	reportOpenSessionAtShutdown(home, classifyExit(0, nil, &code, false, false), newClient)
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
