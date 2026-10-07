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
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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

// NFS mount health is sent as nfs_mounts, reduced to a fixed word: the
// per-share detail /healthz shows (share ID, server, export, mount path,
// mount error) stays on the broker. A failing mount the broker owns
// degrades the status; one it only verifies, or a first pass still
// pending, is reported without degrading it.
func TestHeartbeatHealth_NFSMounts(t *testing.T) {
	cases := []struct {
		name       string
		autoMount  bool
		mountErr   error
		pending    bool
		wantStatus string
		wantNFS    string
	}{
		{name: "mounted", autoMount: true, wantStatus: "healthy", wantNFS: "healthy"},
		{name: "failing mount, broker owns mounts", autoMount: true, mountErr: errors.New("mount 10.0.0.2:/scion-workspaces on /mnt/nfs/ws1 failed: exit status 32 (output: access denied)"), wantStatus: "degraded", wantNFS: "unhealthy"},
		{name: "failing mount, verify only", autoMount: false, mountErr: errors.New("not mounted"), wantStatus: "healthy", wantNFS: "unhealthy"},
		{name: "first pass pending", autoMount: true, mountErr: errors.New("mount failed"), pending: true, wantStatus: "healthy", wantNFS: "unhealthy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newSyncMountChecker()
			mc.mountErr = tc.mountErr
			srv := New(ServerConfig{Host: "127.0.0.1", NFSConfig: nfsCfg(tc.autoMount), NFSMountChecker: mc},
				nil, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
			if !tc.pending {
				_ = srv.nfsMountReconciler.Reconcile(context.Background())
				close(srv.nfsStartupReconcileDone)
			}
			if tc.wantStatus == "degraded" && !srv.nfsHealthDegradesStatus() {
				t.Fatal("expected NFS to degrade the status in this setup")
			}
			if tc.wantNFS == "unhealthy" {
				if raw := srv.GetHealthInfo(context.Background()).Checks["nfs_mounts"]; !strings.Contains(raw, "ws1") {
					t.Fatalf("/healthz nfs_mounts = %q, want the per-share detail kept there", raw)
				}
			}

			heartbeat := sendHealthHeartbeat(t, srv)
			want := &api.BrokerHealthReport{
				Status: tc.wantStatus,
				Checks: map[string]string{"docker": "available", "nfs_mounts": tc.wantNFS},
			}
			if !reflect.DeepEqual(heartbeat.Health, want) {
				t.Errorf("Health = %+v, want %+v", heartbeat.Health, want)
			}
			if heartbeat.Status != "online" {
				t.Errorf("Status = %q, want online", heartbeat.Status)
			}
		})
	}
}
