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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// namedHeartbeatManager is a heartbeatMockManager that reports the name of
// the runtime it lists, as the real AgentManager does, and optionally a
// target ID that differs from the name (as a Kubernetes target's does).
type namedHeartbeatManager struct {
	heartbeatMockManager
	name     string
	targetID string
}

func (m *namedHeartbeatManager) RuntimeName() string     { return m.name }
func (m *namedHeartbeatManager) runtimeTargetID() string { return m.targetID }

func lastHeartbeat(t *testing.T, svc *HeartbeatService, client *mockRuntimeBrokerService) *hubclient.BrokerHeartbeat {
	t.Helper()
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) == 0 {
		t.Fatal("expected a heartbeat call")
	}
	hb := calls[len(calls)-1].Heartbeat
	if hb.Inventory == nil {
		t.Fatal("heartbeat has no inventory")
	}
	return hb
}

func heartbeatAgentTargets(hb *hubclient.BrokerHeartbeat) map[string]string {
	out := map[string]string{}
	for _, p := range hb.Projects {
		for _, a := range p.Agents {
			out[a.Slug] = a.RuntimeTarget
		}
	}
	return out
}

const (
	k8sTargetA = "kubernetes|context=hybval|namespace=default"
	k8sTargetB = "kubernetes|context=hybval|namespace=scion-agents"
)

func TestHeartbeatInventory_CompleteTargetsAndAgentTargets(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "d1", ProjectID: "p1", Phase: "running"},
		}},
		name: "docker",
	}
	auxMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "a1", ProjectID: "p1", Phase: "running"},
		}},
		name:     "kubernetes",
		targetID: k8sTargetB,
	}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	hb := lastHeartbeat(t, svc, client)
	want := []hubclient.InventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: true},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
	}
	if !reflect.DeepEqual(hb.Inventory.Targets, want) {
		t.Errorf("targets = %+v, want %+v", hb.Inventory.Targets, want)
	}
	if got, want := heartbeatAgentTargets(hb), map[string]string{"d1": "docker", "a1": k8sTargetB}; !reflect.DeepEqual(got, want) {
		t.Errorf("agent targets = %v, want %v", got, want)
	}
}

// One forbidden auxiliary listing marks only that target incomplete; the
// other targets stay complete and their agents are still reported.
func TestHeartbeatInventory_TwoTargetsOneForbidden(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{name: "docker"}
	forbidden := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{err: errors.New("pods is forbidden")},
		name:                 "kubernetes",
		targetID:             k8sTargetA,
	}
	allowed := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{agents: []api.AgentInfo{
			{Name: "a1", ProjectID: "p1", Phase: "running"},
		}},
		name:     "kubernetes",
		targetID: k8sTargetB,
	}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{forbidden, allowed} }

	hb := lastHeartbeat(t, svc, client)
	want := []hubclient.InventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: true},
		{ID: k8sTargetA, Runtime: "kubernetes", Complete: false},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
	}
	if !reflect.DeepEqual(hb.Inventory.Targets, want) {
		t.Errorf("targets = %+v, want %+v", hb.Inventory.Targets, want)
	}
	if got := heartbeatAgentTargets(hb)["a1"]; got != k8sTargetB {
		t.Errorf("a1 target = %q, want %q", got, k8sTargetB)
	}
}

func TestHeartbeatInventory_DefaultListFailureOnlyDefaultIncomplete(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{err: errors.New("runtime unavailable")},
		name:                 "docker",
	}
	auxMgr := &namedHeartbeatManager{name: "kubernetes", targetID: k8sTargetB}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	hb := lastHeartbeat(t, svc, client)
	want := []hubclient.InventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: false},
		{ID: k8sTargetB, Runtime: "kubernetes", Complete: true},
	}
	if !reflect.DeepEqual(hb.Inventory.Targets, want) {
		t.Errorf("targets = %+v, want %+v", hb.Inventory.Targets, want)
	}
}

// The same target reported by two managers is complete only when both
// listings succeeded.
func TestHeartbeatInventory_DuplicateTargetMerged(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{name: "kubernetes", targetID: k8sTargetB}
	auxMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{err: errors.New("timeout")},
		name:                 "kubernetes",
		targetID:             k8sTargetB,
	}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	hb := lastHeartbeat(t, svc, client)
	want := []hubclient.InventoryTarget{{ID: k8sTargetB, Runtime: "kubernetes", Complete: false}}
	if !reflect.DeepEqual(hb.Inventory.Targets, want) {
		t.Errorf("targets = %+v, want %+v", hb.Inventory.Targets, want)
	}
}

func TestHeartbeatInventory_UnnamedManagerNoTarget(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	// heartbeatMockManager does not report a runtime name.
	mgr := &heartbeatMockManager{agents: []api.AgentInfo{{Name: "d1", ProjectID: "p1", Phase: "running"}}}
	svc := NewHeartbeatService(client, "b1", time.Hour, mgr, nil, slog.Default())

	hb := lastHeartbeat(t, svc, client)
	if len(hb.Inventory.Targets) != 0 {
		t.Errorf("an unidentified manager must not be reported as a target, got %+v", hb.Inventory.Targets)
	}
	if got := heartbeatAgentTargets(hb)["d1"]; got != "" {
		t.Errorf("agent from an unidentified manager must carry no target, got %q", got)
	}
}

func TestHeartbeatInventory_ProjectFilterClaimsNothing(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{name: "docker"}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, func(string) bool { return true }, slog.Default())

	if hb := lastHeartbeat(t, svc, client); len(hb.Inventory.Targets) != 0 {
		t.Fatalf("a filtered (multi-hub) heartbeat must claim no target, got %+v", hb.Inventory.Targets)
	}
}

// recordingHandler captures log records for assertions.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) count(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level {
			n++
		}
	}
	return n
}

// A failing listing is logged at Warn once per state change, then at Debug.
func TestHeartbeatInventory_ListFailureLoggedOncePerStateChange(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	defaultMgr := &namedHeartbeatManager{name: "docker"}
	auxMgr := &namedHeartbeatManager{
		heartbeatMockManager: heartbeatMockManager{err: errors.New("pods is forbidden")},
		name:                 "kubernetes",
		targetID:             k8sTargetA,
	}
	h := &recordingHandler{}
	svc := NewHeartbeatService(client, "b1", time.Hour, defaultMgr, nil, slog.New(h))
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	for i := 0; i < 3; i++ {
		lastHeartbeat(t, svc, client)
	}
	if got := h.count(slog.LevelWarn); got != 1 {
		t.Fatalf("Warn records after 3 failing heartbeats = %d, want 1", got)
	}
	if got := h.count(slog.LevelDebug); got != 2 {
		t.Errorf("Debug records after 3 failing heartbeats = %d, want 2", got)
	}

	auxMgr.err = nil
	lastHeartbeat(t, svc, client)
	lastHeartbeat(t, svc, client)
	if got := h.count(slog.LevelInfo); got != 1 {
		t.Errorf("Info records after recovery = %d, want 1", got)
	}

	auxMgr.err = errors.New("pods is forbidden")
	lastHeartbeat(t, svc, client)
	if got := h.count(slog.LevelWarn); got != 2 {
		t.Errorf("Warn records after a new failure = %d, want 2", got)
	}
}

// The production manager's target ID is the identity the broker keys its
// auxiliary runtimes by, so a Kubernetes target includes context and
// namespace.
func TestHeartbeatTargetOf_AgentManagerUsesRuntimeIdentity(t *testing.T) {
	rt := &scionrt.KubernetesRuntime{DefaultNamespace: "scion-agents"}
	id, name := heartbeatTargetOf(agent.NewManager(rt))
	if id != auxiliaryRuntimeIdentity(rt) || id != "kubernetes|context=|namespace=scion-agents" {
		t.Errorf("target ID = %q, want %q", id, auxiliaryRuntimeIdentity(rt))
	}
	if name != "kubernetes" {
		t.Errorf("runtime name = %q, want kubernetes", name)
	}
	if id, name := heartbeatTargetOf(&agent.AgentManager{}); id != "" || name != "" {
		t.Errorf("manager without a runtime: got (%q, %q), want empty", id, name)
	}
}
