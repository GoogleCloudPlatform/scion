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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestIsControlChannelConnected_NoConnections_CCNotEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = false

	if !srv.IsControlChannelConnected() {
		t.Error("expected true when no connections and control channel not enabled (Cloud Run path)")
	}
}

func TestIsControlChannelConnected_NoConnections_CCEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = true

	if srv.IsControlChannelConnected() {
		t.Error("expected false when no connections but control channel is enabled")
	}
}

func TestIsControlChannelConnected_Connected(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = true

	cc := NewControlChannelClient(DefaultControlChannelConfig(), nil, nil, "local", nil)
	// Simulate a connected control channel.
	cc.mu.Lock()
	cc.connected = true
	cc.mu.Unlock()

	conn := &HubConnection{
		Name:           "local",
		ControlChannel: cc,
	}

	srv.hubMu.Lock()
	srv.hubConnections["local"] = conn
	srv.hubMu.Unlock()

	if !srv.IsControlChannelConnected() {
		t.Error("expected true when control channel is connected")
	}
}

func TestIsControlChannelConnected_Disconnected(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = true

	cc := NewControlChannelClient(DefaultControlChannelConfig(), nil, nil, "local", nil)
	// connected defaults to false — simulates a disconnected control channel.

	conn := &HubConnection{
		Name:           "local",
		ControlChannel: cc,
	}

	srv.hubMu.Lock()
	srv.hubConnections["local"] = conn
	srv.hubMu.Unlock()

	if srv.IsControlChannelConnected() {
		t.Error("expected false when control channel is disconnected")
	}
}

func TestIsControlChannelConnected_MultipleConnections_OneConnected(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = true

	ccDisconnected := NewControlChannelClient(DefaultControlChannelConfig(), nil, nil, "hub-1", nil)

	ccConnected := NewControlChannelClient(DefaultControlChannelConfig(), nil, nil, "hub-2", nil)
	ccConnected.mu.Lock()
	ccConnected.connected = true
	ccConnected.mu.Unlock()

	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:           "hub-1",
		ControlChannel: ccDisconnected,
	}
	srv.hubConnections["hub-2"] = &HubConnection{
		Name:           "hub-2",
		ControlChannel: ccConnected,
	}
	srv.hubMu.Unlock()

	if !srv.IsControlChannelConnected() {
		t.Error("expected true when at least one control channel is connected")
	}
}

func TestIsControlChannelConnected_NilControlChannel_CCEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = true

	conn := &HubConnection{
		Name:           "local",
		ControlChannel: nil,
	}

	srv.hubMu.Lock()
	srv.hubConnections["local"] = conn
	srv.hubMu.Unlock()

	if srv.IsControlChannelConnected() {
		t.Error("expected false when connection has nil CC and control channel is enabled")
	}
}

func TestIsControlChannelConnected_NilControlChannel_CCNotEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.config.ControlChannelEnabled = false

	conn := &HubConnection{
		Name:           "local",
		ControlChannel: nil,
	}

	srv.hubMu.Lock()
	srv.hubConnections["local"] = conn
	srv.hubMu.Unlock()

	if !srv.IsControlChannelConnected() {
		t.Error("expected true when connections exist without CC and CC is not enabled (Cloud Run)")
	}
}

func TestSwapRuntime(t *testing.T) {
	srv := newTestServer(t)

	if got := srv.RuntimeName(); got != "mock" {
		t.Fatalf("initial runtime = %q, want %q", got, "mock")
	}

	newRT := &runtime.MockRuntime{NameFunc: func() string { return "podman" }}
	srv.SwapRuntime(newRT)

	if got := srv.RuntimeName(); got != "podman" {
		t.Errorf("after swap runtime = %q, want %q", got, "podman")
	}

	if srv.manager == nil {
		t.Error("manager should be re-created after swap")
	}
}
