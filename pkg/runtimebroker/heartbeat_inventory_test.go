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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// namedHeartbeatManager is a heartbeatMockManager that reports the name of
// the runtime it lists, as the real AgentManager does.
type namedHeartbeatManager struct {
	heartbeatMockManager
	name string
}

func (m *namedHeartbeatManager) RuntimeName() string { return m.name }

func inventoryFromHeartbeat(t *testing.T, svc *HeartbeatService, client *mockRuntimeBrokerService) *hubclient.BrokerInventory {
	t.Helper()
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 heartbeat call, got %d", len(calls))
	}
	inv := calls[0].Heartbeat.Inventory
	if inv == nil {
		t.Fatal("heartbeat has no inventory")
	}
	return inv
}

func TestHeartbeatInventory_CompleteListsRuntimes(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{name: "docker"}
	auxMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "a1", ProjectID: "p1", Phase: "running"},
		}},
		name: "kubernetes",
	}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	inv := inventoryFromHeartbeat(t, svc, client)
	if !inv.Complete {
		t.Fatal("expected a complete inventory when every List succeeds")
	}
	if want := []string{"docker", "kubernetes"}; !reflect.DeepEqual(inv.Runtimes, want) {
		t.Errorf("runtimes = %v, want %v", inv.Runtimes, want)
	}
}

func TestHeartbeatInventory_AuxiliaryListFailureIncomplete(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "a1", ProjectID: "p1", Phase: "running"},
		}},
		name: "docker",
	}
	auxMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{err: errors.New("api unavailable")},
		name:                 "kubernetes",
	}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	inv := inventoryFromHeartbeat(t, svc, client)
	if inv.Complete {
		t.Fatal("an auxiliary List failure must mark the inventory incomplete")
	}
	if len(inv.Runtimes) != 0 {
		t.Errorf("incomplete inventory should not list runtimes, got %v", inv.Runtimes)
	}
}

func TestHeartbeatInventory_DefaultListFailureIncomplete(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{err: errors.New("runtime unavailable")},
		name:                 "docker",
	}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())

	if inv := inventoryFromHeartbeat(t, svc, client); inv.Complete {
		t.Fatal("a default List failure must mark the inventory incomplete")
	}
}

func TestHeartbeatInventory_UnnamedRuntimeIncomplete(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	// heartbeatMockManager does not report a runtime name.
	svc := NewHeartbeatService(client, "b1", time.Hour, &heartbeatMockManager{}, nil, slog.Default())

	if inv := inventoryFromHeartbeat(t, svc, client); inv.Complete {
		t.Fatal("a manager without a runtime name must mark the inventory incomplete")
	}
}

func TestHeartbeatInventory_ProjectFilterIncomplete(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{name: "docker"}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, func(string) bool { return true }, slog.Default())

	if inv := inventoryFromHeartbeat(t, svc, client); inv.Complete {
		t.Fatal("a filtered (multi-hub) heartbeat must not claim a complete inventory")
	}
}

// The production manager must report its runtime name, or the inventory is
// never complete and the hub never reconciles missing agents.
var _ runtimeNamer = (*agent.AgentManager)(nil)
