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
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/sshd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// countSSHBuilds counts newConduitSSH calls for the test.
func countSSHBuilds(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := newConduitSSH
	newConduitSSH = func(u conduit.PTYUser, getenv func(string) string) (*sshd.Config, error) {
		n.Add(1)
		return orig(u, getenv)
	}
	t.Cleanup(func() { newConduitSSH = orig })
	return &n
}

// runPortForwarding runs p until the test ends.
func runPortForwarding(t *testing.T, p portForwarding) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(pfWait):
			t.Error("run did not return after cancel")
		}
	})
}

// TestConduitSSHOnlyWithConduit: with hub.conduit off no SSH server (and
// no host key) is created and nothing is advertised; with it on, one
// server is created and the Hello advertises the ssh kind.
func TestConduitSSHOnlyWithConduit(t *testing.T) {
	t.Run("conduit off", func(t *testing.T) {
		n := countSSHBuilds(t)
		h := newPFHub(t, true)
		client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
		env := map[string]string{envProjectID: "project-1", conduit.EnvLaunchID: "launch-1"}
		runPortForwarding(t, newPortForwarding(client, false, conduit.PTYUser{}, func(k string) string { return env[k] }))
		waitSignal(t, h.tunnel, "the legacy tunnel")
		if got := n.Load(); got != 0 {
			t.Fatalf("SSH server built %d times with conduit off", got)
		}
		if h.conduitHits.Load() != 0 {
			t.Fatal("conduit dialed with conduit off")
		}
	})
	t.Run("conduit on", func(t *testing.T) {
		t.Cleanup(hub.SetTokenHome(t.TempDir()))
		n := countSSHBuilds(t)
		h := newPFHub(t, true)
		client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
		env := map[string]string{
			conduit.EnvHubExperiments: conduit.ExperimentConduit,
			envProjectID:              "project-1",
			conduit.EnvLaunchID:       "launch-1",
		}
		runPortForwarding(t, newPortForwarding(client, false, conduit.PTYUser{}, func(k string) string { return env[k] }))
		select {
		case hello := <-h.hellos:
			if kinds := hello.GetCapabilities().GetStreamKinds(); !slices.Contains(kinds, grant.StreamKindSSH) {
				t.Fatalf("Hello stream kinds %v lack ssh", kinds)
			}
		case <-time.After(pfWait):
			t.Fatal("no conduit Hello")
		}
		if got := n.Load(); got != 1 {
			t.Fatalf("SSH server built %d times, want 1", got)
		}
	})
}

// TestSSHMaxChannelsEnv: SCION_CONDUIT_SSH_MAX_CHANNELS sets the cap; an
// unset or invalid value keeps the default.
func TestSSHMaxChannelsEnv(t *testing.T) {
	for raw, want := range map[string]int{"": 32, "8": 8, "64": 64, "0": 32, "-1": 32, "x": 32} {
		got := sshMaxChannels(func(k string) string {
			if k == envSSHMaxChannels {
				return raw
			}
			return ""
		})
		if got != want {
			t.Errorf("%s=%q: cap %d, want %d", envSSHMaxChannels, raw, got, want)
		}
	}
}
