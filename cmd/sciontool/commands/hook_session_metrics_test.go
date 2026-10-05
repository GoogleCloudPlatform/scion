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
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// fakeMetricsHub records the session-metrics reports it receives and
// accepts every other request (status updates and the like).
type fakeMetricsHub struct {
	mu      sync.Mutex
	reports []hub.MetricsPayload
}

func (f *fakeMetricsHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/hook-test-agent/metrics" {
		var p hub.MetricsPayload
		if err := json.Unmarshal(body, &p); err == nil {
			f.mu.Lock()
			f.reports = append(f.reports, p)
			f.mu.Unlock()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (f *fakeMetricsHub) Reports() []hub.MetricsPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hub.MetricsPayload(nil), f.reports...)
}

// startDiscardingReceiver starts a local OTLP receiver that accepts and
// drops everything, so the hook process's synchronous exporters succeed
// quickly. It returns the gRPC port.
func startDiscardingReceiver(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &telemetry.Config{Enabled: true, GRPCPort: port}
	r := telemetry.NewReceiver(cfg,
		func(context.Context, []*tracepb.ResourceSpans) error { return nil },
		telemetry.WithMetricHandler(func(context.Context, []*metricpb.ResourceMetrics) error { return nil }),
		telemetry.WithLogHandler(func(context.Context, []*logspb.ResourceLogs) error { return nil }),
	)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return port
}

// Each harness hook event runs `sciontool hook` in a new process with a new
// telemetry handler. A session's events, delivered that way, must produce
// exactly one session-metrics report to the Hub, carrying the harness
// session ID and the counts from every event.
func TestProcessHookData_SessionMetricsReportedAcrossHookRuns(t *testing.T) {
	home := t.TempDir()
	scrubScionEnv(t)
	t.Setenv("HOME", home)
	log.SetLogPath(filepath.Join(home, "agent.log"))

	fake := &fakeMetricsHub{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "hook-test-agent")
	t.Setenv("SCION_TELEMETRY_ENABLED", "true")
	t.Setenv("SCION_TELEMETRY_CLOUD_ENABLED", "false")
	t.Setenv("SCION_OTEL_GRPC_PORT", strconv.Itoa(startDiscardingReceiver(t)))

	hookDialect = "claude"
	statePath := filepath.Join(home, ".scion", handlers.SessionStateFileName)

	events := []map[string]interface{}{
		{"hook_event_name": "SessionStart", "session_id": "sess-hook-1", "source": "startup"},
		{"hook_event_name": "PostToolUse", "session_id": "sess-hook-1", "tool_name": "Bash"},
		{"hook_event_name": "PostToolUse", "session_id": "sess-hook-1", "tool_name": "Bash"},
		{"hook_event_name": "PostToolUse", "session_id": "sess-hook-1", "tool_name": "Read"},
		{"hook_event_name": "Stop", "session_id": "sess-hook-1"},
		{"hook_event_name": "Stop", "session_id": "sess-hook-1"},
		{"hook_event_name": "SessionEnd", "session_id": "sess-hook-1", "reason": "logout"},
	}
	for i, ev := range events {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := processHookData(data); err != nil {
			t.Fatalf("processHookData(%v): %v", ev["hook_event_name"], err)
		}
		if i == 1 {
			if info, err := os.Stat(statePath); err != nil {
				t.Errorf("state file after first tool event: %v", err)
			} else if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("state file mode = %o, want 600", mode)
			}
		}
	}

	reports := fake.Reports()
	if len(reports) != 1 {
		t.Fatalf("got %d session-metrics reports, want 1: %+v", len(reports), reports)
	}
	p := reports[0]
	if p.Session.ID != "sess-hook-1" {
		t.Errorf("session.id = %q, want sess-hook-1", p.Session.ID)
	}
	if p.Session.TurnCount != 2 {
		t.Errorf("session.turn_count = %d, want 2", p.Session.TurnCount)
	}
	if got := p.Tools["Bash"].Calls; got != 2 {
		t.Errorf("tools[Bash].calls = %d, want 2", got)
	}
	if got := p.Tools["Read"].Calls; got != 1 {
		t.Errorf("tools[Read].calls = %d, want 1", got)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file still present after session-end (err=%v)", err)
	}
}

// Without a configured Hub client nothing is persisted or reported.
func TestWireSessionMetrics_NoHubClient(t *testing.T) {
	h := handlers.NewTelemetryHandler(nil, nil, nil)
	wireSessionMetrics(h, hub.NewClientWithConfig("", "", ""), t.TempDir())
	if h.SessionState != nil || h.OnSessionEnd != nil {
		t.Fatal("session metrics wired without a configured Hub client")
	}
	wireSessionMetrics(h, hub.NewClientWithConfig("http://hub.invalid", "tok", "agent"), "")
	if h.SessionState != nil || h.OnSessionEnd != nil {
		t.Fatal("session metrics wired without a home directory")
	}
}
