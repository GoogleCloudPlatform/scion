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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// TestServer_VersionIsBuildToken pins that the broker reports the same
// build token as the hub (version.Short()), not a placeholder, so the hub
// can compare them (ptone/scion#5972).
func TestServer_VersionIsBuildToken(t *testing.T) {
	prevCommit, prevVersion := version.Commit, version.Version
	version.Commit, version.Version = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567", ""
	t.Cleanup(func() { version.Commit, version.Version = prevCommit, prevVersion })
	srv := New(DefaultServerConfig(), &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	if srv.version != "0a1b2c3d" {
		t.Fatalf("Server.version = %q, want the pinned commit's build token %q", srv.version, "0a1b2c3d")
	}
}

// TestHeartbeatService_ReportsVersion pins that every heartbeat carries the
// version given to SetVersion, and that an unset version is omitted.
func TestHeartbeatService_ReportsVersion(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())

	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	svc.SetVersion("v1.2.3")
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 heartbeat calls, got %d", len(calls))
	}
	if got := calls[0].Heartbeat.Version; got != "" {
		t.Errorf("heartbeat before SetVersion: Version = %q, want empty", got)
	}
	if got := calls[1].Heartbeat.Version; got != "v1.2.3" {
		t.Errorf("heartbeat after SetVersion: Version = %q, want %q", got, "v1.2.3")
	}
}
