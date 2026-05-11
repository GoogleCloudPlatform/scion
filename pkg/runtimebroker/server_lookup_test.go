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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// filteringMockManager implements agent.Manager with label-based filtering.
type filteringMockManager struct {
	mockManager
}

func (m *filteringMockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if filter == nil {
		return m.agents, nil
	}
	var result []api.AgentInfo
	for _, a := range m.agents {
		match := true
		for k, v := range filter {
			if a.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			result = append(result, a)
		}
	}
	return result, nil
}

func TestLookupContainerID_DefaultManager(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "abc123",
			Name:        "myagent",
			Labels:      map[string]string{"scion.name": "myagent"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	containerID, err := srv.LookupContainerID(context.Background(), "myagent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerID != "abc123" {
		t.Errorf("expected abc123, got %s", containerID)
	}
}

func TestLookupContainerID_CaseInsensitive(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "abc123",
			Name:        "myagent",
			Labels:      map[string]string{"scion.name": "myagent"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	containerID, err := srv.LookupContainerID(context.Background(), "MyAgent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerID != "abc123" {
		t.Errorf("expected abc123, got %s", containerID)
	}
}

func TestLookupContainerID_NotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupContainerID(context.Background(), "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
	if got := err.Error(); got != "agent 'nonexistent' not found" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestLookupContainerID_FallbackToAuxiliary(t *testing.T) {
	// Default manager has no agents
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	// Auxiliary manager (kubernetes) has the agent
	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{
		{
			ContainerID: "k8s-pod-xyz",
			Name:        "k8sagent",
			Labels:      map[string]string{"scion.name": "k8sagent"},
		},
	}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	// Add auxiliary runtime
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: auxRt,
		Manager: agent.NewManager(auxRt),
	}
	// Override with our mock manager that has agents
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: auxRt,
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	containerID, err := srv.LookupContainerID(context.Background(), "k8sagent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerID != "k8s-pod-xyz" {
		t.Errorf("expected k8s-pod-xyz, got %s", containerID)
	}
}

func TestLookupAgent_DefaultRuntime(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-1",
			Name:        "agent1",
			Labels:      map[string]string{"scion.name": "agent1"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result, err := srv.LookupAgent(context.Background(), "agent1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "container-1" {
		t.Errorf("expected container-1, got %s", result.ContainerID)
	}
	if result.RuntimeName != "docker" {
		t.Errorf("expected docker runtime, got %s", result.RuntimeName)
	}
	if result.Namespace != "" {
		t.Errorf("expected empty namespace for docker, got %s", result.Namespace)
	}
	if result.K8sConfig != nil {
		t.Error("expected nil K8sConfig for docker runtime")
	}
}

func TestLookupAgent_K8sAuxiliaryRuntime(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{
		{
			ContainerID: "k8s-pod-1",
			Name:        "k8sagent",
			Labels:      map[string]string{"scion.name": "k8sagent"},
			Kubernetes:  &api.AgentK8sMetadata{Namespace: "scion-ns", PodName: "k8s-pod-1"},
		},
	}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: auxRt,
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	result, err := srv.LookupAgent(context.Background(), "k8sagent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "k8s-pod-1" {
		t.Errorf("expected k8s-pod-1, got %s", result.ContainerID)
	}
	if result.RuntimeName != "kubernetes" {
		t.Errorf("expected kubernetes runtime, got %s", result.RuntimeName)
	}
	if result.Namespace != "scion-ns" {
		t.Errorf("expected scion-ns namespace, got %s", result.Namespace)
	}
}

func TestLookupAgent_NotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupAgent(context.Background(), "ghost", "")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestLookupAgent_PrefersContainerIDLabel(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "runtime-id",
			ID:          "agent-uuid",
			Name:        "agent1",
			Labels: map[string]string{
				"scion.name":         "agent1",
				"scion.container.id": "label-container-id",
			},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result, err := srv.LookupAgent(context.Background(), "agent1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "label-container-id" {
		t.Errorf("expected label-container-id, got %s", result.ContainerID)
	}
}

func TestLookupAgent_FallsBackToContainerID(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "runtime-id",
			Name:        "agent1",
			Labels:      map[string]string{"scion.name": "agent1"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result, err := srv.LookupAgent(context.Background(), "agent1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "runtime-id" {
		t.Errorf("expected runtime-id, got %s", result.ContainerID)
	}
}

func TestResolveManagerForAgent_DefaultManager(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			Name:   "myagent",
			Labels: map[string]string{"scion.name": "myagent"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result := srv.resolveManagerForAgent(context.Background(), "myagent", "")
	if result != mgr {
		t.Error("expected default manager to be returned")
	}
}

func TestResolveManagerForAgent_FallbackToAuxiliary(t *testing.T) {
	// Default manager has no agents
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	// Auxiliary manager (kubernetes) has the agent
	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{
		{
			Name:   "k8sagent",
			Labels: map[string]string{"scion.name": "k8sagent"},
		},
	}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: auxRt,
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	result := srv.resolveManagerForAgent(context.Background(), "k8sagent", "")
	if result != auxMgr {
		t.Error("expected auxiliary manager to be returned for k8s agent")
	}
}

func TestResolveManagerForAgent_NotFoundFallsBackToDefault(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	result := srv.resolveManagerForAgent(context.Background(), "nonexistent", "")
	if result != defaultMgr {
		t.Error("expected default manager when agent not found anywhere")
	}
}

func TestResolveManagerForAgent_CaseInsensitive(t *testing.T) {
	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{
		{
			Name:   "myagent",
			Labels: map[string]string{"scion.name": "myagent"},
		},
	}

	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: auxRt,
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	result := srv.resolveManagerForAgent(context.Background(), "MyAgent", "")
	if result != auxMgr {
		t.Error("expected auxiliary manager to be returned for case-insensitive lookup")
	}
}

func TestResolveRuntimeForAgent_DefaultRuntime(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			Name:   "myagent",
			Labels: map[string]string{"scion.name": "myagent"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result := srv.resolveRuntimeForAgent(context.Background(), "myagent", "")
	if result != rt {
		t.Error("expected default runtime to be returned")
	}
}

func TestResolveRuntimeForAgent_FallbackToAuxiliary(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{
		{
			Name:   "k8sagent",
			Labels: map[string]string{"scion.name": "k8sagent"},
		},
	}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: auxRt,
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	result := srv.resolveRuntimeForAgent(context.Background(), "k8sagent", "")
	if result != auxRt {
		t.Error("expected auxiliary runtime to be returned for k8s agent")
	}
}

func TestResolveRuntimeForAgent_NotFoundFallsBackToDefault(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	result := srv.resolveRuntimeForAgent(context.Background(), "nonexistent", "")
	if result != rt {
		t.Error("expected default runtime when agent not found anywhere")
	}
}

func TestRuntimeCommand_ReturnsRuntimeName(t *testing.T) {
	rt := &runtime.MockRuntime{NameFunc: func() string { return "podman" }}
	srv := New(DefaultServerConfig(), &mockManager{}, rt)

	if got := srv.RuntimeCommand(); got != "podman" {
		t.Errorf("expected podman, got %s", got)
	}
}

func TestRuntimeCommand_DefaultFallback(t *testing.T) {
	srv := New(DefaultServerConfig(), &mockManager{}, nil)

	if got := srv.RuntimeCommand(); got != "docker" {
		t.Errorf("expected docker fallback, got %s", got)
	}
}

func TestLookupContainerID_ProjectScopedDisambiguation(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-ggcloud",
			Name:        "foobar",
			Labels:      map[string]string{"scion.name": "foobar", "scion.grove_id": "grove-aaa"},
		},
		{
			ContainerID: "container-muskateers",
			Name:        "foobar",
			Labels:      map[string]string{"scion.name": "foobar", "scion.grove_id": "grove-bbb"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	// With grove scoping, should get the correct container
	id, err := srv.LookupContainerID(context.Background(), "foobar", "grove-aaa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "container-ggcloud" {
		t.Errorf("expected container-ggcloud, got %s", id)
	}

	id, err = srv.LookupContainerID(context.Background(), "foobar", "grove-bbb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "container-muskateers" {
		t.Errorf("expected container-muskateers, got %s", id)
	}
}

func TestLookupAgent_ProjectScopedDisambiguation(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-ggcloud",
			Name:        "foobar",
			Labels:      map[string]string{"scion.name": "foobar", "scion.grove_id": "grove-aaa"},
		},
		{
			ContainerID: "container-storytree",
			Name:        "foobar",
			Labels:      map[string]string{"scion.name": "foobar", "scion.grove_id": "grove-ccc"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result, err := srv.LookupAgent(context.Background(), "foobar", "grove-ccc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "container-storytree" {
		t.Errorf("expected container-storytree, got %s", result.ContainerID)
	}
}

func TestLookupAgent_ProjectFallbackForLegacyContainers(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "legacy-container",
			Name:        "oldagent",
			Labels:      map[string]string{"scion.name": "oldagent"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	// Should still find agents without scion.grove_id via fallback
	result, err := srv.LookupAgent(context.Background(), "oldagent", "some-grove-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "legacy-container" {
		t.Errorf("expected legacy-container, got %s", result.ContainerID)
	}
}
