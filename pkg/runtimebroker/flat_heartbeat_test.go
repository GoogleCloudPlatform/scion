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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
