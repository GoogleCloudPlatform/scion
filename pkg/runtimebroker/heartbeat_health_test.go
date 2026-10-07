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

package runtimebroker

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// sendHealthHeartbeat sends one heartbeat with the server's health wired
// in, as HubConnection.Start does, and returns it.
func sendHealthHeartbeat(t *testing.T, srv *Server) *hubclient.BrokerHeartbeat {
	t.Helper()
	client := &mockRuntimeBrokerService{}
	hb := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())
	hb.health = srv.heartbeatHealthReport
	if err := hb.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 heartbeat call, got %d", len(calls))
	}
	return calls[0].Heartbeat
}

// A broker whose default runtime failed to start reports itself degraded
// with runtime "unavailable", and its heartbeat still says online: health
// never replaces liveness.
func TestHeartbeatHealth_FailingDefaultRuntimeSendsDegraded(t *testing.T) {
	heartbeat := sendHealthHeartbeat(t, newErrorRuntimeTestServer(t))

	want := &api.BrokerHealthReport{
		Status: "degraded",
		Checks: map[string]string{"runtime": "unavailable"},
	}
	if !reflect.DeepEqual(heartbeat.Health, want) {
		t.Errorf("Health = %+v, want %+v", heartbeat.Health, want)
	}
	if heartbeat.Status != "online" {
		t.Errorf("Status = %q, want online (health must not change liveness)", heartbeat.Status)
	}
}

// A healthy broker reports the same checks as /healthz.
func TestHeartbeatHealth_HealthyRuntime(t *testing.T) {
	srv := newTestServer(t)
	heartbeat := sendHealthHeartbeat(t, srv)

	want := &api.BrokerHealthReport{
		Status: "healthy",
		Checks: map[string]string{"mock": "available"},
	}
	if !reflect.DeepEqual(heartbeat.Health, want) {
		t.Errorf("Health = %+v, want %+v", heartbeat.Health, want)
	}
	info := srv.GetHealthInfo(context.Background())
	if info.Status != heartbeat.Health.Status || !reflect.DeepEqual(info.Checks, heartbeat.Health.Checks) {
		t.Errorf("heartbeat health %+v differs from /healthz %+v", heartbeat.Health, info)
	}
}

// The report is a copy: changing it does not change what GetHealthInfo
// builds next, and vice versa.
func TestHeartbeatHealthReport_CopiesChecks(t *testing.T) {
	srv := newTestServer(t)
	report := srv.heartbeatHealthReport(context.Background())
	report.Checks["mock"] = "changed"
	if got := srv.heartbeatHealthReport(context.Background()).Checks["mock"]; got != "available" {
		t.Errorf("checks[mock] = %q after mutating an earlier report, want available", got)
	}
}

// Without a health source (a service not started from a hub connection)
// the field is omitted, as an older broker would.
func TestHeartbeatHealth_OmittedWithoutSource(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	hb := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())
	if err := hb.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 heartbeat call, got %d", len(calls))
	}
	if calls[0].Heartbeat.Health != nil {
		t.Errorf("Health = %+v, want nil", calls[0].Heartbeat.Health)
	}
}
