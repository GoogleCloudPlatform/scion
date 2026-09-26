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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
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
			actual := a.Labels[k]
			// Mirror every real runtime's List: the project_id filter key
			// also matches the legacy grove_id label.
			if actual == "" && k == projectcompat.LabelProjectID {
				actual = projectcompat.ProjectIDFromLabels(a.Labels)
			}
			if actual != v {
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

func TestResolveAgentRuntimeTarget_ProjectScopedAuxiliary(t *testing.T) {
	defaultMgr := &filteringMockManager{mockManager: mockManager{agents: []api.AgentInfo{
		{Name: "shared-name", Labels: map[string]string{"scion.name": "shared-name", "scion.project_id": "project-a"}},
	}}}
	auxMgr := &filteringMockManager{mockManager: mockManager{agents: []api.AgentInfo{
		{Name: "shared-name", Labels: map[string]string{"scion.name": "shared-name", "scion.project_id": "project-b"}},
	}}}
	defaultRuntime := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRuntime := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, defaultRuntime)
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRuntime, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	manager, selectedRuntime := srv.resolveAgentRuntimeTarget(context.Background(), "Shared-Name", "project-b")
	if manager != auxMgr {
		t.Error("expected the project-scoped auxiliary manager")
	}
	if selectedRuntime != auxRuntime {
		t.Error("expected the runtime paired with the project-scoped auxiliary manager")
	}
	if got := srv.resolveManagerForAgent(context.Background(), "Shared-Name", "project-b"); got != auxMgr {
		t.Error("manager wrapper selected a different backend")
	}
	if got := srv.resolveRuntimeForAgent(context.Background(), "Shared-Name", "project-b"); got != auxRuntime {
		t.Error("runtime wrapper selected a different backend")
	}
}

func TestResolveAgentRuntimeTarget_ProjectFallbackSkipsOtherProjects(t *testing.T) {
	defaultMgr := &filteringMockManager{mockManager: mockManager{agents: []api.AgentInfo{
		{Name: "shared-name", Labels: map[string]string{"scion.name": "shared-name", "scion.project_id": "project-a"}},
	}}}
	auxMgr := &filteringMockManager{mockManager: mockManager{agents: []api.AgentInfo{
		// Legacy project metadata does not match the canonical filter, so this
		// backend is discovered only by the compatibility fallback.
		{Name: "shared-name", Labels: map[string]string{"scion.name": "shared-name", "scion.grove_id": "project-b"}},
	}}}
	defaultRuntime := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRuntime := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, defaultRuntime)
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRuntime, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	manager, selectedRuntime := srv.resolveAgentRuntimeTarget(context.Background(), "shared-name", "project-b")
	if manager != auxMgr || selectedRuntime != auxRuntime {
		t.Error("expected fallback to skip the same-name agent from another project")
	}
}

func TestResolveAgentRuntimeTarget_ProjectFallbackAcceptsUnlabeledAgent(t *testing.T) {
	manager := &filteringMockManager{mockManager: mockManager{agents: []api.AgentInfo{
		{Name: "legacy-agent", Labels: map[string]string{"scion.name": "legacy-agent"}},
	}}}
	runtime := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), manager, runtime)

	selectedManager, selectedRuntime := srv.resolveAgentRuntimeTarget(context.Background(), "legacy-agent", "project-a")
	if selectedManager != manager || selectedRuntime != runtime {
		t.Error("expected fallback to retain support for unlabeled agents")
	}
}

func TestHasAgentInProjectOrUnlabeled(t *testing.T) {
	tests := []struct {
		name  string
		agent api.AgentInfo
		match bool
	}{
		{
			name:  "canonical label matches",
			agent: api.AgentInfo{Labels: map[string]string{"scion.project_id": "project-a"}},
			match: true,
		},
		{
			name:  "legacy label matches",
			agent: api.AgentInfo{Labels: map[string]string{"scion.grove_id": "project-a"}},
			match: true,
		},
		{
			name:  "agent field matches without label",
			agent: api.AgentInfo{ProjectID: "project-a"},
			match: true,
		},
		{
			name:  "explicit other project",
			agent: api.AgentInfo{ProjectID: "project-b"},
		},
		{
			name:  "label takes precedence over agent field",
			agent: api.AgentInfo{Labels: map[string]string{"scion.project_id": "project-b"}, ProjectID: "project-a"},
		},
		{
			name:  "unlabeled compatibility",
			agent: api.AgentInfo{},
			match: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasAgentInProjectOrUnlabeled([]api.AgentInfo{test.agent}, "project-a"); got != test.match {
				t.Fatalf("hasAgentInProjectOrUnlabeled() = %v, want %v", got, test.match)
			}
		})
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

func TestLookupContainerID_DifferentProjectNotMatchedViaFallback(t *testing.T) {
	// A labeled container in grove-aaa must NOT be returned for a grove-bbb
	// request via the backward-compat fallback — that would be a cross-project
	// collision. The fallback is only for genuinely unlabeled containers.
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-aaa",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-aaa"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	if _, err := srv.LookupContainerID(context.Background(), "coordinator", "grove-bbb"); err == nil {
		t.Error("expected error: a different project's labeled agent must not match via fallback")
	}

	if _, err := srv.LookupAgent(context.Background(), "coordinator", "grove-bbb"); err == nil {
		t.Error("expected error from LookupAgent: different project's labeled agent must not match via fallback")
	}
}

// TestLookupAgent_AuxiliaryListErrorSurfacesUnavailable proves that an
// auxiliary runtime's List failure (e.g. an intermittent `docker ps` or
// apiserver error) must not be indistinguishable from "no such agent". The
// default manager finds nothing
// (a real signal the agent isn't there via the default runtime), but the
// only auxiliary runtime consulted couldn't answer at all — so the overall
// result must be ErrAgentListUnavailable (which the broker's PTY classifier
// and stream-open path turn into 4503, retry), never a plain "not found"
// (which would become a terminal 4404).
func TestLookupAgent_AuxiliaryListErrorSurfacesUnavailable(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &mockManager{listErr: errors.New("docker ps: connection refused")}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	_, err := srv.LookupAgent(context.Background(), "ghost", "")
	if err == nil {
		t.Fatal("expected an error when the auxiliary runtime's List call fails")
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected ErrAgentListUnavailable, got: %v", err)
	}
}

// scopedThenFailManager succeeds (empty, no error) for a project-scoped List
// filter and fails for the unscoped fallback filter (`{"scion.name": slug}`,
// used by LookupAgent's backward-compatibility retry). This lets a test drive
// LookupAgent's second, project-fallback code path specifically — a manager
// that fails on every filter would already trip the first, project-scoped
// loop, which sets listUnavailable itself and passes even if the fallback
// loop's own check is broken (that's exactly the gap the reviewer's mutation
// found: removing the fallback loop's `listUnavailable = true` left every
// TestLookupAgent* test green).
type scopedThenFailManager struct {
	mockManager
	failErr error
}

func (m *scopedThenFailManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if filter[projectcompat.LabelProjectID] != "" {
		return nil, nil
	}
	return nil, m.failErr
}

// TestLookupAgent_AuxiliaryListErrorInProjectFallbackSurfacesUnavailable is
// the same regression as TestLookupAgent_AuxiliaryListErrorSurfacesUnavailable,
// but isolated to the project-scoped backward-compatibility fallback path
// (the second aux loop in LookupAgent, `server.go` around line 1277): the
// auxiliary runtime here succeeds (empty) for the first, project-scoped aux
// loop, so only the second loop's List failure can be the source of the
// ErrAgentListUnavailable this test requires.
func TestLookupAgent_AuxiliaryListErrorInProjectFallbackSurfacesUnavailable(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &scopedThenFailManager{failErr: errors.New("apiserver: context deadline exceeded")}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	_, err := srv.LookupAgent(context.Background(), "ghost", "some-project")
	if err == nil {
		t.Fatal("expected an error when the fallback aux runtime's List call fails")
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected ErrAgentListUnavailable, got: %v", err)
	}
}

// TestLookupAgent_PrimaryManagerFallbackListErrorSurfacesUnavailable covers
// the primary (non-auxiliary) manager's own fallback List call: LookupAgent's
// backward-compatibility retry (`agents, err = s.manager.List(ctx,
// fallbackFilter)`) previously dropped a non-nil err on the floor and fell
// through to "not found" instead of propagating it. No auxiliary runtimes are
// registered, isolating this specific call.
func TestLookupAgent_PrimaryManagerFallbackListErrorSurfacesUnavailable(t *testing.T) {
	mgr := &scopedThenFailManager{failErr: errors.New("list: connection reset")}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupAgent(context.Background(), "ghost", "some-project")
	if err == nil {
		t.Fatal("expected an error when the primary manager's fallback List call fails")
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected ErrAgentListUnavailable, got: %v", err)
	}
}

// TestLookupAgent_NoAuxiliaryRuntimesStillReportsNotFound guards against
// over-correction: when nothing errors and nothing matches, LookupAgent must
// still report a plain not-found (which the broker maps to a terminal 4404),
// not ErrAgentListUnavailable.
func TestLookupAgent_NoAuxiliaryRuntimesStillReportsNotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupAgent(context.Background(), "ghost", "")
	if err == nil {
		t.Fatal("expected an error for a nonexistent agent")
	}
	if errors.Is(err, ErrAgentListUnavailable) {
		t.Error("a genuine not-found must not be reported as ErrAgentListUnavailable")
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

// countingListManager wraps filteringMockManager to count how many times
// List is called, so a test can prove an aux loop stopped after its first
// match instead of unconditionally consulting every registered runtime.
type countingListManager struct {
	filteringMockManager
	listCalls int
}

func (m *countingListManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.listCalls++
	return m.filteringMockManager.List(ctx, filter)
}

// TestLookupAgent_AuxiliaryLoopStopsAfterFirstMatch covers the auxiliary-
// runtime loop's early exit: once one auxiliary runtime's List call matches
// the slug, the loop must not go on to call List on any other registered
// runtime. Map iteration order is random, so both auxiliary runtimes here
// are set up to match — whichever one iteration reaches first should be the
// only one queried, regardless of which that turns out to be.
func TestLookupAgent_AuxiliaryLoopStopsAfterFirstMatch(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxA := &countingListManager{}
	auxA.agents = []api.AgentInfo{{
		ContainerID: "aux-a-container",
		Name:        "twoaux",
		Labels:      map[string]string{"scion.name": "twoaux"},
	}}
	auxB := &countingListManager{}
	auxB.agents = []api.AgentInfo{{
		ContainerID: "aux-b-container",
		Name:        "twoaux",
		Labels:      map[string]string{"scion.name": "twoaux"},
	}}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRtA := &runtime.MockRuntime{NameFunc: func() string { return "runtime-a" }}
	auxRtB := &runtime.MockRuntime{NameFunc: func() string { return "runtime-b" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["runtime-a"] = auxiliaryRuntime{Runtime: auxRtA, Manager: auxA}
	srv.auxiliaryRuntimes["runtime-b"] = auxiliaryRuntime{Runtime: auxRtB, Manager: auxB}
	srv.auxiliaryRuntimesMu.Unlock()

	result, err := srv.LookupAgent(context.Background(), "twoaux", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	totalCalls := auxA.listCalls + auxB.listCalls
	if totalCalls != 1 {
		t.Fatalf("expected exactly one auxiliary List call once a match is found, got %d (auxA=%d, auxB=%d)",
			totalCalls, auxA.listCalls, auxB.listCalls)
	}

	// Whichever aux was actually queried must be the one the result came
	// from, and the other must never have been touched.
	switch {
	case auxA.listCalls == 1:
		if result.ContainerID != "aux-a-container" {
			t.Errorf("expected aux-a-container (the queried runtime), got %s", result.ContainerID)
		}
		if auxB.listCalls != 0 {
			t.Errorf("aux runtime-b's List should not have been called, got %d calls", auxB.listCalls)
		}
	case auxB.listCalls == 1:
		if result.ContainerID != "aux-b-container" {
			t.Errorf("expected aux-b-container (the queried runtime), got %s", result.ContainerID)
		}
		if auxA.listCalls != 0 {
			t.Errorf("aux runtime-a's List should not have been called, got %d calls", auxA.listCalls)
		}
	}
}
