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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// P1.3 part 2 (ptone/scion#3269): a flat Runtime Broker instance's
// heartbeat stays on its saved identity and single target.

// profileScopedHeartbeat builds a heartbeat service whose sources all report
// profile-scoped data, a primary target with an agent that carries a
// profile, and an auxiliary target with another agent.
func profileScopedHeartbeat(flat bool) (*HeartbeatService, *mockRuntimeBrokerService) {
	client := &mockRuntimeBrokerService{}
	primary := &namedHeartbeatManager{name: "docker", targetID: "docker", heartbeatMockManager: heartbeatMockManager{
		agents: []api.AgentInfo{{Name: "flat-agent", ProjectID: "p1", Phase: "running", Profile: "batch"}},
	}}
	aux := &namedHeartbeatManager{name: "kubernetes", targetID: "kubernetes/ctx/ns", heartbeatMockManager: heartbeatMockManager{
		agents: []api.AgentInfo{{Name: "aux-agent", ProjectID: "p1", Phase: "running", Profile: "cluster"}},
	}}
	svc := NewHeartbeatService(client, "flat-broker-id", time.Hour, primary, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{aux} }
	name := "batch"
	svc.defaultProfile = func() *string { return &name }
	svc.profileAttach = func() []hubclient.ProfileAttachState {
		return []hubclient.ProfileAttachState{{Name: "batch", Attach: true}}
	}
	svc.profileSAMappings = func() []hubclient.ProfileSAMappingsState {
		return []hubclient.ProfileSAMappingsState{{Name: "cluster", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{{GSA: "gsa@example.iam"}}}}
	}
	svc.flat = flat
	return svc, client
}

func reportedAgents(hb *hubclient.BrokerHeartbeat) map[string]hubclient.AgentHeartbeat {
	out := map[string]hubclient.AgentHeartbeat{}
	for _, p := range hb.Projects {
		for _, a := range p.Agents {
			out[a.Slug] = a
		}
	}
	return out
}

// TestFlatHeartbeat_OwnIdentitySingleTargetNoProfiles: a flat instance's
// heartbeat is sent under its own Runtime Broker ID, lists only its single
// runtime target, and carries no profile-scoped data.
func TestFlatHeartbeat_OwnIdentitySingleTargetNoProfiles(t *testing.T) {
	svc, client := profileScopedHeartbeat(true)
	hb := lastHeartbeat(t, svc, client)
	calls := client.getHeartbeatCalls()
	if got := calls[len(calls)-1].BrokerID; got != "flat-broker-id" {
		t.Fatalf("heartbeat sent under %q, want the instance's own Runtime Broker ID", got)
	}
	if hb.DefaultProfile != nil {
		t.Errorf("DefaultProfile = %q, want none", *hb.DefaultProfile)
	}
	if len(hb.ProfileAttach) != 0 {
		t.Errorf("ProfileAttach = %+v, want none", hb.ProfileAttach)
	}
	if len(hb.ProfileSAMappings) != 0 {
		t.Errorf("ProfileSAMappings = %+v, want none", hb.ProfileSAMappings)
	}
	if len(hb.Inventory.Targets) != 1 || hb.Inventory.Targets[0].ID != "docker" || !hb.Inventory.Targets[0].Complete {
		t.Errorf("inventory targets = %+v, want only the instance's complete target", hb.Inventory.Targets)
	}
	agents := reportedAgents(hb)
	if _, ok := agents["aux-agent"]; ok {
		t.Error("an auxiliary runtime's agent was reported by a flat instance")
	}
	a, ok := agents["flat-agent"]
	if !ok {
		t.Fatalf("the instance's agent is missing: %+v", agents)
	}
	if a.Profile != "" {
		t.Errorf("agent profile = %q, want none", a.Profile)
	}
	if a.RuntimeTarget != "docker" {
		t.Errorf("agent runtime target = %q, want the inventory target key", a.RuntimeTarget)
	}
}

// TestFlatHeartbeat_LegacyHeartbeatUnchanged: the same sources on a legacy
// Runtime Broker still report profile-scoped data and every target.
func TestFlatHeartbeat_LegacyHeartbeatUnchanged(t *testing.T) {
	svc, client := profileScopedHeartbeat(false)
	hb := lastHeartbeat(t, svc, client)
	if hb.DefaultProfile == nil || *hb.DefaultProfile != "batch" {
		t.Errorf("DefaultProfile = %v, want batch", hb.DefaultProfile)
	}
	if len(hb.ProfileAttach) != 1 {
		t.Errorf("ProfileAttach = %+v, want the reported profile", hb.ProfileAttach)
	}
	if len(hb.ProfileSAMappings) != 1 {
		t.Errorf("ProfileSAMappings = %+v, want the reported mappings", hb.ProfileSAMappings)
	}
	if len(hb.Inventory.Targets) != 2 {
		t.Errorf("inventory targets = %+v, want both targets", hb.Inventory.Targets)
	}
	agents := reportedAgents(hb)
	if agents["flat-agent"].Profile != "batch" || agents["aux-agent"].Profile != "cluster" {
		t.Errorf("agent profiles = %+v, want the reported profiles", agents)
	}
}

// TestFlatHubConnection_RunsOnlyUnderOwnIdentity: a flat instance starts no
// heartbeat or control channel for a hub connection that carries another
// Runtime Broker ID; its own connection, with the same credentials and hub
// client, starts a flat-mode heartbeat sent under the instance's ID.
func TestFlatHubConnection_RunsOnlyUnderOwnIdentity(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	cfg := f.srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false
	f.srv.config = cfg
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Both connections have valid credentials and a hub client, so only the
	// Runtime Broker ID decides whether a heartbeat starts.
	conn := func(name, brokerID string) (*HubConnection, *mockRuntimeBrokerService) {
		brokers := &mockRuntimeBrokerService{}
		return &HubConnection{
			Name:        name,
			BrokerID:    brokerID,
			HubClient:   &stubBrokerHubClient{brokers: brokers},
			Credentials: makeTestCreds(name, brokerID, f.srv.config.HubEndpoint),
		}, brokers
	}

	other, otherBrokers := conn("other", legacyBrokerID)
	err := other.Start(ctx, f.srv)
	if err == nil || !strings.Contains(err.Error(), f.identity.RuntimeBrokerID) {
		t.Fatalf("Start with another Runtime Broker ID: err = %v, want a refusal naming the instance's ID", err)
	}
	if other.Heartbeat != nil {
		t.Fatal("a heartbeat was started under another Runtime Broker ID")
	}

	own, ownBrokers := conn("own", f.identity.RuntimeBrokerID)
	if err := own.Start(ctx, f.srv); err != nil {
		t.Fatalf("Start under the instance's own ID: %v", err)
	}
	defer own.Stop()
	if own.Heartbeat == nil {
		t.Fatal("no heartbeat started under the instance's own ID")
	}
	if own.Heartbeat.brokerID != f.identity.RuntimeBrokerID || !own.Heartbeat.flat {
		t.Fatalf("heartbeat brokerID=%q flat=%v, want flat mode under %q", own.Heartbeat.brokerID, own.Heartbeat.flat, f.identity.RuntimeBrokerID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(ownBrokers.getHeartbeatCalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := ownBrokers.getHeartbeatCalls()
	if len(calls) == 0 {
		t.Fatal("the own connection sent no heartbeat")
	}
	if calls[0].BrokerID != f.identity.RuntimeBrokerID || calls[0].Heartbeat.DefaultProfile != nil {
		t.Fatalf("first heartbeat sent under %q with DefaultProfile %v, want the instance's ID and no profile", calls[0].BrokerID, calls[0].Heartbeat.DefaultProfile)
	}
	if n := len(otherBrokers.getHeartbeatCalls()); n != 0 {
		t.Fatalf("the refused connection sent %d heartbeats", n)
	}
}

// TestFlatHubConnection_HeartbeatServiceMode: the heartbeat service a flat
// instance builds for its hub connection is in flat mode; a legacy Runtime
// Broker's is not.
func TestFlatHubConnection_HeartbeatServiceMode(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	hb := f.srv.newHeartbeatService(&mockRuntimeBrokerService{}, f.identity.RuntimeBrokerID, f.srv.config.HubEndpoint, time.Hour)
	if !hb.flat || hb.brokerID != f.identity.RuntimeBrokerID {
		t.Fatalf("flat instance heartbeat: flat=%v brokerID=%q, want flat mode under %q", hb.flat, hb.brokerID, f.identity.RuntimeBrokerID)
	}

	legacy := newTestServerWithManager(t, &mockManager{})
	if lhb := legacy.newHeartbeatService(&mockRuntimeBrokerService{}, "legacy", "", time.Hour); lhb.flat {
		t.Fatal("a legacy Runtime Broker's heartbeat is in flat mode")
	}
}

// countingAuxResolver records every auxiliary-runtime resolution and answers
// with a Kubernetes-named runtime distinct from the default.
type countingAuxResolver struct {
	mu       sync.Mutex
	profiles []string
}

func (r *countingAuxResolver) resolve(projectPath, agentName, profile string) scionrt.Runtime {
	r.mu.Lock()
	r.profiles = append(r.profiles, profile)
	r.mu.Unlock()
	return &scionrt.MockRuntime{NameFunc: func() string { return "kubernetes" }}
}

func (r *countingAuxResolver) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.profiles...)
}

func auxRuntimeCount(s *Server) int {
	s.auxiliaryRuntimesMu.RLock()
	defer s.auxiliaryRuntimesMu.RUnlock()
	return len(s.auxiliaryRuntimes)
}

// TestFlatServerStart_NeverResolvesAuxiliaryProfiles (ptone/scion#3605
// assessment): a flat instance's Start never resolves auxiliary profiles,
// even when the project settings carry a remote-style Kubernetes profile;
// its auxiliary runtimes stay empty. A legacy server started from the same
// settings does discover it (control).
func TestFlatServerStart_NeverResolvesAuxiliaryProfiles(t *testing.T) {
	// The fixture writes a "remote" profile on a kubernetes runtime into the
	// global and project settings.
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true, activeProfile: "remote"})

	start := func(t *testing.T, srv *Server) (context.CancelFunc, chan error) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- srv.Start(ctx) }()
		t.Cleanup(func() {
			cancel()
			_ = srv.Shutdown(context.Background())
		})
		return cancel, errCh
	}

	t.Run("flat", func(t *testing.T) {
		cfg := f.srv.config
		cfg.HeartbeatEnabled = true // readiness: hub connections start after the discovery step
		cfg.ControlChannelEnabled = false
		srv := New(cfg, f.mgr, f.srv.runtime)
		res := &countingAuxResolver{}
		srv.resolveAuxiliaryRuntime = res.resolve
		_, errCh := start(t, srv)

		deadline := time.Now().Add(10 * time.Second)
		started := false
		for !started && time.Now().Before(deadline) {
			select {
			case err := <-errCh:
				t.Fatalf("Start returned early: %v", err)
			default:
			}
			srv.hubMu.RLock()
			for _, c := range srv.hubConnections {
				c.mu.RLock()
				started = started || c.Heartbeat != nil
				c.mu.RUnlock()
			}
			srv.hubMu.RUnlock()
			time.Sleep(20 * time.Millisecond)
		}
		if !started {
			t.Fatal("the flat server did not reach its hub-connection start (after the discovery step)")
		}
		if got := res.calls(); len(got) != 0 {
			t.Fatalf("a flat instance resolved auxiliary profiles %v; it must resolve none", got)
		}
		if n := auxRuntimeCount(srv); n != 0 {
			t.Fatalf("a flat instance registered %d auxiliary runtimes, want 0", n)
		}
	})

	t.Run("legacy control", func(t *testing.T) {
		cfg := f.srv.config
		cfg.FlatInstance = nil
		cfg.HubEnabled = false
		srv := New(cfg, f.mgr, f.srv.runtime)
		res := &countingAuxResolver{}
		srv.resolveAuxiliaryRuntime = res.resolve
		start(t, srv)

		deadline := time.Now().Add(10 * time.Second)
		for auxRuntimeCount(srv) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := auxRuntimeCount(srv); n == 0 {
			t.Fatalf("control: a legacy server must discover the remote profile's runtime (resolver calls %v)", res.calls())
		}
		found := false
		for _, p := range res.calls() {
			found = found || p == "remote"
		}
		if !found {
			t.Fatalf("control: resolver calls %v, want the remote profile", res.calls())
		}
	})
}
