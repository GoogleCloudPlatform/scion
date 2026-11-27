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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

func writeHubOutboxActiveMarker(t *testing.T, home string) {
	t.Helper()
	hubDir := telemetry.HubAnalyticsOutboxDirFor(home)
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatalf("mkdir hub outbox: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, "active"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write active marker: %v", err)
	}
}

type markerHeldStore struct {
	due []telemetry.SessionSummary
}

func (m *markerHeldStore) ClaimDueHeldReportsNoFollow() ([]telemetry.SessionSummary, error) {
	out := m.due
	m.due = nil
	return out, nil
}
func (m *markerHeldStore) CompleteReportsNoFollow(...telemetry.SessionSummary) error { return nil }
func (m *markerHeldStore) RetryReportsNoFollow(...telemetry.SessionSummary) error    { return nil }

type markerReporter struct {
	sent []hub.MetricsPayload
}

func (r *markerReporter) ReportMetrics(_ context.Context, p hub.MetricsPayload) error {
	r.sent = append(r.sent, p)
	return nil
}

func TestWireSessionMetrics_UsageEventsEnabledMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{name: "hub sink inactive omits marker", active: false},
		{name: "hub sink active sets usage_events_enabled", active: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.active {
				writeHubOutboxActiveMarker(t, home)
			}

			var gotPayload hub.MetricsPayload
			var received bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
					t.Errorf("decode payload: %v", err)
				}
				received = true
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
			t.Setenv("SCION_AUTH_TOKEN", "tok-test")
			t.Setenv("SCION_AGENT_ID", "agent-marker-1")
			t.Setenv("SCION_USAGE_SOURCE", "hooks")

			client := hub.NewClient()
			h := handlers.NewTelemetryHandler(nil, nil, nil)
			wireSessionMetrics(context.Background(), h, client, home)

			_ = h.Handle(&hooks.Event{
				Name: hooks.EventSessionStart,
				Data: hooks.EventData{SessionID: "sess-marker-1"},
			})
			_ = h.Handle(&hooks.Event{
				Name: hooks.EventSessionEnd,
				Data: hooks.EventData{SessionID: "sess-marker-1", InputTokens: 100, OutputTokens: 50},
			})

			if !received {
				t.Fatal("expected metrics POST to hub")
			}
			if gotPayload.UsageEventsEnabled != tc.active {
				t.Fatalf("UsageEventsEnabled = %v, want %v", gotPayload.UsageEventsEnabled, tc.active)
			}
		})
	}
}

func TestSendDueHeldReports_UsageEventsEnabledMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{name: "inactive", active: false},
		{name: "active", active: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.active {
				writeHubOutboxActiveMarker(t, home)
			}
			st := &markerHeldStore{
				due: []telemetry.SessionSummary{{
					SessionID:   "held-sess-1",
					AgentID:     "agent-1",
					StartedAt:   time.Now().Add(-time.Minute),
					EndedAt:     time.Now(),
					TokensInput: 42,
				}},
			}
			rep := &markerReporter{}
			sendDueHeldReports(context.Background(), st, home, func() metricsReporter { return rep })
			if len(rep.sent) != 1 {
				t.Fatalf("sent %d payloads, want 1", len(rep.sent))
			}
			if rep.sent[0].UsageEventsEnabled != tc.active {
				t.Fatalf("UsageEventsEnabled = %v, want %v", rep.sent[0].UsageEventsEnabled, tc.active)
			}
		})
	}
}

func TestReportOpenSessionAtShutdown_UsageEventsEnabledMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{name: "inactive", active: false},
		{name: "active", active: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.active {
				writeHubOutboxActiveMarker(t, home)
			}

			var gotPayload hub.MetricsPayload
			var received bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
					t.Errorf("decode payload: %v", err)
				}
				received = true
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
			t.Setenv("SCION_AUTH_TOKEN", "tok-test")
			t.Setenv("SCION_AGENT_ID", "agent-shutdown-1")
			t.Setenv("SCION_USAGE_SOURCE", "hooks")

			// Open a session in the session state file without ending it.
			h := handlers.NewTelemetryHandler(nil, nil, nil)
			h.SessionState = handlers.NewFileSessionState(home)
			_ = h.Handle(&hooks.Event{
				Name: hooks.EventSessionStart,
				Data: hooks.EventData{SessionID: "sess-shutdown-1"},
			})

			reportOpenSessionAtShutdown(home, exitOutcome{}, hub.NewClient)

			if !received {
				t.Fatal("expected shutdown metrics POST to hub")
			}
			if gotPayload.UsageEventsEnabled != tc.active {
				t.Fatalf("UsageEventsEnabled = %v, want %v", gotPayload.UsageEventsEnabled, tc.active)
			}
		})
	}
}
