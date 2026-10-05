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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// Pins today's wiring for ptone/scion#3248: the init daemon registers
// NewLifecycleTelemetryHandler on the LifecycleManager for lifecycle
// events only (registerLifecycleTelemetryHandler in
// cmd/sciontool/commands/init.go), so its aggregator never sees a
// harness hook event. The session-end it gets
// has no session ID, and the report is refused before any request.
// When ptone/scion#3248 is fixed, this test should change to expect a
// report with a session ID.
func TestLifecycleTelemetryHandler_CurrentWiringSendsNoReport(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := hub.NewClientWithConfig(server.URL, "test-token", "agent-1")

	manager := hooks.NewLifecycleManager()
	manager.HooksDirs = []string{t.TempDir()} // no script hooks
	handler := NewLifecycleTelemetryHandler(nil, nil, nil)
	for _, eventName := range []string{hooks.EventPreStart, hooks.EventPostStart, hooks.EventPreStop, hooks.EventSessionEnd} {
		manager.RegisterHandler(eventName, handler.Handle)
	}

	var summaries []telemetry.SessionSummary
	var reportErr error
	handler.OnSessionEnd = func(summary telemetry.SessionSummary) {
		summaries = append(summaries, summary)
		reportErr = client.ReportMetrics(context.Background(), hub.SummaryToMetricsPayload(summary))
	}

	if err := manager.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	if err := manager.RunPostStart(); err != nil {
		t.Fatalf("RunPostStart: %v", err)
	}
	if err := manager.RunSessionEnd(); err != nil {
		t.Fatalf("RunSessionEnd: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("OnSessionEnd called %d times, want 1", len(summaries))
	}
	if summaries[0].SessionID != "" || summaries[0].APICallCount != 0 {
		t.Errorf("got id=%q api=%d, want no ID and no counts", summaries[0].SessionID, summaries[0].APICallCount)
	}
	if reportErr == nil || reportErr.Error() != "session metrics not sent: summary has no session ID" {
		t.Errorf("ReportMetrics error = %v, want the no-session-ID error", reportErr)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("hub received %d requests, want 0", n)
	}
}
