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

//go:build !no_sqlite

package relayserver

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// TestRoleRecheckSweepRunsOnTheConfiguredClock: the relay role's re-check
// sweep (the timer that also drives the stream scheduler's sweep) runs on
// Config.Clock: with a fake clock its timer is armed one RecheckInterval
// after start on that clock, and advancing the clock by that interval arms
// the next one. A role on another clock arms neither.
func TestRoleRecheckSweepRunsOnTheConfiguredClock(t *testing.T) {
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	// The relay's heartbeat timer runs on the same fake clock; the sweep's
	// deadlines must not coincide with a heartbeat deadline (the hub.conduit
	// poll is an hour out in testConfig).
	cfg.RecheckInterval = 13 * time.Second
	if relay.DefaultHeartbeatInterval%cfg.RecheckInterval == 0 || cfg.RecheckInterval%relay.DefaultHeartbeatInterval == 0 {
		t.Fatalf("sweep period %v shares deadlines with the heartbeat interval %v", cfg.RecheckInterval, relay.DefaultHeartbeatInterval)
	}
	clk := cfg.Clock.(*clock.Fake)
	start := clk.Now()
	_ = startServer(t, cfg)
	if !clk.WaitForTimer(10*time.Second, start.Add(cfg.RecheckInterval)) {
		t.Fatal("the role's sweep timer is not on the configured clock")
	}
	clk.Advance(cfg.RecheckInterval)
	if !clk.WaitForTimer(10*time.Second, start.Add(2*cfg.RecheckInterval)) {
		t.Fatal("advancing the configured clock did not arm the next sweep")
	}
}

// TestNilClockDefaultsToReal: with no Clock, the server (and so the role it
// starts with that clock) runs on real time.
func TestNilClockDefaultsToReal(t *testing.T) {
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	cfg.Clock = nil
	s := startServer(t, cfg)
	if s.cfg.Clock != clock.Real() {
		t.Fatalf("clock %T, want the real clock", s.cfg.Clock)
	}
}
