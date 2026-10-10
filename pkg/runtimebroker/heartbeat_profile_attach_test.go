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
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/require"
)

// The heartbeat carries the provider's per-profile attach state, and omits
// the field entirely when there is no provider.
func TestHeartbeat_ReportsProfileAttach(t *testing.T) {
	hb := NewHeartbeatService(&mockRuntimeBrokerService{}, "test-host", time.Hour, &mockManager{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got := hb.buildHeartbeat(context.Background())
	require.Nil(t, got.ProfileAttach, "no profile attach state without a provider")
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "profileAttach", "an unset field is omitted from the payload")

	want := []hubclient.ProfileAttachState{
		{Name: "local", Attach: true},
		{Name: "remote", Attach: false},
	}
	hb.profileAttach = func() []hubclient.ProfileAttachState { return want }
	got = hb.buildHeartbeat(context.Background())
	require.Equal(t, want, got.ProfileAttach)

	raw, err = json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"profileAttach":[{"name":"local","attach":true},{"name":"remote","attach":false}]`)
}

// heartbeatProfileAttach reports each profile with a known attach state and
// leaves out a profile with no live runtime to ask (Attach == nil).
func TestServer_HeartbeatProfileAttach(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
  podman:
    type: podman
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
  other:
    runtime: podman
`)

	srv := &Server{
		runtime:           &runtime.MockRuntime{NameFunc: func() string { return "docker" }},
		auxiliaryRuntimes: map[string]auxiliaryRuntime{},
	}
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: newOptOutTestRuntime("kubernetes"),
		Manager: &mockManager{},
	}

	got := srv.heartbeatProfileAttach()
	require.Equal(t, []hubclient.ProfileAttachState{
		{Name: "local", Attach: true},
		{Name: "remote", Attach: false},
	}, got, "the podman profile has no live runtime and is left out")
}

// A hub connection's heartbeat service is wired to report profile attach
// state.
func TestHubConnectionStart_HeartbeatReportsProfileAttach(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
runtimes:
  docker:
    type: docker
profiles:
  local:
    runtime: docker
`)
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)
	cfg := srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false
	srv.config = cfg

	srv.hubMu.RLock()
	conn, ok := srv.hubConnections["local"]
	srv.hubMu.RUnlock()
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, conn.Start(ctx, srv))

	conn.mu.RLock()
	hb := conn.Heartbeat
	conn.mu.RUnlock()
	require.NotNil(t, hb)
	got := hb.buildHeartbeat(ctx)
	require.Equal(t, []hubclient.ProfileAttachState{{Name: "local", Attach: true}}, got.ProfileAttach,
		"the docker profile backed by the default runtime is reported")
}
