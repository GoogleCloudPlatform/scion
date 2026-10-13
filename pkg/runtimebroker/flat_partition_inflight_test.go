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
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Partition validation (ptone/scion#3274): starts in flight, async launches and their
// recovery stay per instance on a shared daemon.

// TestFlatPartition_StartsInFlightAndLaunchesStayPerInstance: instance B
// has a start and an async launch in flight for its agent; A's stop and
// delete of that name never cancel them, A's in-flight view and heartbeat
// never report them, and B's do.
func TestFlatPartition_StartsInFlightAndLaunchesStayPerInstance(t *testing.T) {
	f := newPartitionFixture(t)
	key := launchKey{ProjectID: "proj-1", Slug: "helper"}

	startCtx, finishStart := f.b.srv.startsInFlight.beginRun(context.Background(), key, "run-b-start")
	defer finishStart()
	launchCtx, cancelLaunch := context.WithCancel(context.Background())
	defer cancelLaunch()
	rec := newLaunchRecord("launch-b", "agent-b2", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancelLaunch)
	rec.RunID = "run-b-launch"
	f.b.srv.launchRegistry.Begin(key, rec)
	defer f.b.srv.launchRegistry.Finish(key, rec)

	serveFlat(f.a.srv, http.MethodPost, "/api/v1/agents/helper/stop?projectId=proj-1", "")
	serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/helper?projectId=proj-1&deleteFiles=true", "")
	serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/helper?projectId=proj-1&runId=run-b-launch", "")

	if startCtx.Err() != nil {
		t.Error("A's stop or delete cancelled B's start in flight")
	}
	if launchCtx.Err() != nil {
		t.Error("A's stop or delete cancelled B's async launch")
	}
	for _, k := range f.a.srv.startsInFlightSnapshot() {
		if k == key {
			t.Errorf("A reports B's start in flight: %v", f.a.srv.startsInFlightSnapshot())
		}
	}
	found := false
	for _, k := range f.b.srv.startsInFlightSnapshot() {
		found = found || k == key
	}
	if !found {
		t.Error("B does not report its own start in flight")
	}

	for _, tc := range []struct {
		p    *partitionInstance
		want bool
	}{{f.a, false}, {f.b, true}} {
		svc := NewHeartbeatService(&mockRuntimeBrokerService{}, tc.p.identity.RuntimeBrokerID, time.Hour, tc.p.mgr, nil, slog.Default())
		svc.flat = true
		svc.startsInFlight = tc.p.srv.startsInFlightSnapshot
		hb := svc.buildHeartbeat(context.Background())
		got := false
		for _, s := range hb.StartsInFlight {
			got = got || (s.ProjectID == "proj-1" && s.Slug == "helper")
		}
		if got != tc.want {
			t.Errorf("%s heartbeat reports B's start in flight = %v, want %v", tc.p.key, got, tc.want)
		}
	}
	assertCalls(t, f.d)
}

// TestFlatPartition_LaunchesAreNotRecoveredAcrossInstancesOrRestart: launch
// records are per server and in memory, so after a restart neither A nor
// a replacement reports or resumes a launch of B (or its own old one).
func TestFlatPartition_LaunchesAreNotRecoveredAcrossInstancesOrRestart(t *testing.T) {
	f := newPartitionFixture(t)
	key := launchKey{ProjectID: "proj-1", Slug: "helper"}
	_, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	rec := newLaunchRecord("launch-b", "agent-b2", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancelB)
	f.b.srv.launchRegistry.Begin(key, rec)
	defer f.b.srv.launchRegistry.Finish(key, rec)
	keyA := launchKey{ProjectID: "proj-1", Slug: "worker"}
	_, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	f.a.srv.launchRegistry.Begin(keyA, newLaunchRecord("launch-a", "agent-a1", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancelA))

	a2 := newPartitionInstance(t, f.d, "docker-a", f.a.stateDir)
	c := newPartitionInstance(t, f.d, "docker-c", t.TempDir())
	for _, p := range []*partitionInstance{a2, c} {
		if keys := p.srv.startsInFlightSnapshot(); len(keys) != 0 {
			t.Errorf("%s recovered launches after a restart: %v", p.key, keys)
		}
		if p.srv.launchRegistry.OtherRunInFlight(key, "") {
			t.Errorf("%s sees B's launch", p.key)
		}
	}
}

// TestFlatPartition_StateRootIsPerRuntimeBrokerID: with no explicit state
// dir (production), each instance's server state root is
// DefaultStateDir(its Runtime Broker ID), its ownership records live there,
// and a restart reuses it.
func TestFlatPartition_StateRootIsPerRuntimeBrokerID(t *testing.T) {
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	a := newPartitionInstance(t, d, "docker-a", "")
	b := newPartitionInstance(t, d, "docker-b", "")
	wantA, err := DefaultStateDir(a.identity.RuntimeBrokerID)
	if err != nil {
		t.Fatal(err)
	}
	wantB, _ := DefaultStateDir(b.identity.RuntimeBrokerID)
	if a.srv.stateDir != wantA || b.srv.stateDir != wantB || wantA == wantB {
		t.Fatalf("state roots A=%q B=%q, want %q and %q (distinct)", a.srv.stateDir, b.srv.stateDir, wantA, wantB)
	}
	if a.srv.ownership.dir != filepath.Join(wantA, "ownership") {
		t.Fatalf("A's records are at %q, want under its state root", a.srv.ownership.dir)
	}
	a.own(t, d, "proj-1", "agent-a1", "worker", "cid-a1")
	a2 := newPartitionInstance(t, d, "docker-a", "")
	if a2.srv.stateDir != wantA {
		t.Fatalf("restart state root %q, want %q", a2.srv.stateDir, wantA)
	}
	if _, ok, err := a2.srv.ownership.Get("proj-1", "agent-a1"); err != nil || !ok {
		t.Fatalf("A's record after restart: %v %v", ok, err)
	}
	if _, ok, _ := b.srv.ownership.Get("proj-1", "agent-a1"); ok {
		t.Fatal("B sees A's record")
	}
}
