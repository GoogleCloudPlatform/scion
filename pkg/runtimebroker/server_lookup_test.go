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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// filteringMockManager implements agent.Manager with label-based filtering.
type filteringMockManager struct {
	mockManager
}

func (m *filteringMockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
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
		// No project_id label at all, so this backend is discovered only by
		// the unlabeled-compatibility fallback.
		{Name: "shared-name", Labels: map[string]string{"scion.name": "shared-name"}},
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
			name:  "canonical label mismatch",
			agent: api.AgentInfo{Labels: map[string]string{"scion.project_id": "project-b"}},
			match: false,
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
			Labels:      map[string]string{"scion.name": "foobar", "scion.project_id": "project-aaa"},
		},
		{
			ContainerID: "container-muskateers",
			Name:        "foobar",
			Labels:      map[string]string{"scion.name": "foobar", "scion.project_id": "project-bbb"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	// With project scoping, should get the correct container
	id, err := srv.LookupContainerID(context.Background(), "foobar", "project-aaa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "container-ggcloud" {
		t.Errorf("expected container-ggcloud, got %s", id)
	}

	id, err = srv.LookupContainerID(context.Background(), "foobar", "project-bbb")
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
			Labels:      map[string]string{"scion.name": "foobar", "scion.project_id": "project-aaa"},
		},
		{
			ContainerID: "container-storytree",
			Name:        "foobar",
			Labels:      map[string]string{"scion.name": "foobar", "scion.project_id": "project-ccc"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	result, err := srv.LookupAgent(context.Background(), "foobar", "project-ccc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContainerID != "container-storytree" {
		t.Errorf("expected container-storytree, got %s", result.ContainerID)
	}
}

func TestLookupContainerID_DifferentProjectNotMatchedViaFallback(t *testing.T) {
	// A labeled container in project-aaa must NOT be returned for a
	// project-bbb request via the backward-compat fallback — that would be a
	// cross-project collision. The fallback is only for genuinely unlabeled
	// containers.
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-aaa",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-aaa"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	if _, err := srv.LookupContainerID(context.Background(), "coordinator", "project-bbb"); err == nil {
		t.Error("expected error: a different project's labeled agent must not match via fallback")
	}

	if _, err := srv.LookupAgent(context.Background(), "coordinator", "project-bbb"); err == nil {
		t.Error("expected error from LookupAgent: different project's labeled agent must not match via fallback")
	}
}

// TestHandleAgentAttach_GroveIDQueryParamNotHonoured verifies that a
// groveId-only query no longer scopes the PTY attach lookup: two same-slug
// agents in different projects are ambiguous (unscoped) rather than the
// groveId value picking out just one of them.
func TestHandleAgentAttach_GroveIDQueryParamNotHonoured(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-a",
			Name:        "shared-agent",
			Labels:      map[string]string{"scion.name": "shared-agent", "scion.project_id": "project-a"},
		},
		{
			ContainerID: "container-b",
			Name:        "shared-agent",
			Labels:      map[string]string{"scion.name": "shared-agent", "scion.project_id": "project-b"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/shared-agent/attach?groveId=project-a", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")

	rec := httptest.NewRecorder()
	srv.handleAgentAttach(rec, req)

	// If groveId still scoped the lookup, only the project-a agent would
	// match and the handler would proceed past LookupAgent. Since it doesn't,
	// both agents match by slug alone and LookupAgent fails closed on the
	// ambiguity, which surfaces here as Not Found.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (ambiguous match proves groveId did not scope the lookup), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", rec.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeAgentNotFound {
		t.Errorf("expected code %q, got %q", ErrCodeAgentNotFound, resp.Error.Code)
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
	if filter[projectkeys.LabelProjectID] != "" {
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

	// Should still find agents without scion.project_id via fallback
	result, err := srv.LookupAgent(context.Background(), "oldagent", "some-project-id")
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
// runtime. Iteration is over a snapshot sorted by identity key (see
// sortedAuxiliaryRuntimes), so which runtime that could answer gets queried
// is deterministic — the lexicographically-first key — not dependent on Go's
// randomized map order. This matters because more than one auxiliary
// runtime can share a type, so in overlap cases more than one could
// plausibly answer for the same slug. Run repeatedly to guard against that
// nondeterminism creeping back in.
//
// Eight registered runtimes are used here, every one of which would answer a
// match if queried, rather than two: with only two entries, a missing sort
// still picks the correct key about half the time by chance alone. With
// eight, chance alone picks the lexicographically-first key only about 1 run
// in 8, so a missing sort fails essentially every run here.
func TestLookupAgent_AuxiliaryLoopStopsAfterFirstMatch(t *testing.T) {
	keys := []string{"runtime-a", "runtime-b", "runtime-c", "runtime-d", "runtime-e", "runtime-f", "runtime-g", "runtime-h"}

	for i := 0; i < 10; i++ {
		defaultMgr := &filteringMockManager{}
		defaultMgr.agents = []api.AgentInfo{}

		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(DefaultServerConfig(), defaultMgr, rt)

		managers := make(map[string]*countingListManager, len(keys))
		srv.auxiliaryRuntimesMu.Lock()
		for _, key := range keys {
			mgr := &countingListManager{}
			mgr.agents = []api.AgentInfo{{
				ContainerID: key + "-container",
				Name:        "manyaux",
				Labels:      map[string]string{"scion.name": "manyaux"},
			}}
			managers[key] = mgr
			auxRT := &runtime.MockRuntime{NameFunc: func() string { return key }}
			srv.auxiliaryRuntimes[key] = auxiliaryRuntime{Runtime: auxRT, Manager: mgr}
		}
		srv.auxiliaryRuntimesMu.Unlock()

		result, err := srv.LookupAgent(context.Background(), "manyaux", "")
		if err != nil {
			t.Fatalf("run %d: unexpected error: %v", i, err)
		}

		for _, key := range keys {
			wantCalls := 0
			if key == "runtime-a" {
				wantCalls = 1
			}
			if got := managers[key].listCalls; got != wantCalls {
				t.Fatalf("run %d: expected %q to be queried %d time(s) (sorted order, stop at first match), got %d", i, key, wantCalls, got)
			}
		}
		if want := "runtime-a-container"; result.ContainerID != want {
			t.Fatalf("run %d: expected %s (the deterministically-first runtime), got %s", i, want, result.ContainerID)
		}
	}
}

// TestLookupAgent_RuntimeNameFromIdentityKeyedAux asserts that when the aux
// runtime is registered under an IDENTITY-shaped key (as
// discoverAuxiliaryRuntimesForProjects and resolveManagerForOpts do — see
// auxiliaryRuntimeIdentity), not the bare type string "kubernetes",
// AgentLookupResult.RuntimeName and the K8sConfig/K8sClientset attachment
// that gates on it still come from the matched runtime's own Name(), not
// from re-using that map key: using the identity string as RuntimeName
// would silently drop K8sConfig.
func TestLookupAgent_RuntimeNameFromIdentityKeyedAux(t *testing.T) {
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
	auxRt := fakeKubernetesRuntime("cluster-a", "scion-ns")
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	identity := auxiliaryRuntimeIdentity(auxRt)
	if identity == "kubernetes" {
		t.Fatalf("test setup invalid: identity %q must differ from the bare type string for this regression test to mean anything", identity)
	}

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[identity] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	result, err := srv.LookupAgent(context.Background(), "k8sagent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RuntimeName != "kubernetes" {
		t.Errorf("expected RuntimeName %q (the runtime's own Name()), got %q (the identity map key, or something else)", "kubernetes", result.RuntimeName)
	}
	if result.Namespace != "scion-ns" {
		t.Errorf("expected scion-ns namespace, got %s", result.Namespace)
	}
}

// TestLookupAgent_RuntimeNameFromIdentityKeyedAux_ProjectFallbackBranch is
// the same regression as TestLookupAgent_RuntimeNameFromIdentityKeyedAux,
// exercised via the SECOND aux-runtime loop (the unlabeled-fallback branch
// reached when the project-scoped lookup finds nothing) — the two loops
// duplicate the runtimeName-assignment logic, so each needs its own test.
func TestLookupAgent_RuntimeNameFromIdentityKeyedAux_ProjectFallbackBranch(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{
		{
			ContainerID: "k8s-pod-legacy",
			Name:        "k8sagent",
			// No project label: only matched via the unlabeled-fallback path.
			Labels:     map[string]string{"scion.name": "k8sagent"},
			Kubernetes: &api.AgentK8sMetadata{Namespace: "scion-ns", PodName: "k8s-pod-legacy"},
		},
	}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := fakeKubernetesRuntime("cluster-a", "scion-ns")
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	identity := auxiliaryRuntimeIdentity(auxRt)
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[identity] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	// A non-empty projectID forces the project-scoped attempt to miss first
	// (the aux agent carries no scion.project_id label), then fall through
	// to the unlabeled-fallback loop.
	result, err := srv.LookupAgent(context.Background(), "k8sagent", "some-project-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RuntimeName != "kubernetes" {
		t.Errorf("expected RuntimeName %q, got %q", "kubernetes", result.RuntimeName)
	}
}

// TestLookupContainerID_NoContainerIDIsErrAgentNotFound proves the "matched
// record but no container id" classification directly: LookupContainerID
// must satisfy errors.Is(err, ErrAgentNotFound) so restartAgent/stopAgent
// fold it into the idempotent not-found path (skip stop, proceed to start).
// It also checks the error message identifies the no-container-ID branch,
// since the generic agent-not-found path also satisfies errors.Is(err,
// ErrAgentNotFound) and would otherwise let a plain no-match pass.
func TestLookupContainerID_NoContainerIDIsErrAgentNotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			Name:   "coordinator",
			Labels: map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupContainerID(context.Background(), "coordinator", "project-A")
	if err == nil {
		t.Fatal("expected an error for a matched agent with no container id")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected errors.Is(err, ErrAgentNotFound), got: %v", err)
	}
	if !strings.Contains(err.Error(), "no container ID") {
		t.Errorf("expected error to identify the no-container-ID branch, got: %v", err)
	}
}

// TestLookupContainerID_AmbiguousMatchIsNotErrAgentNotFound proves the
// opposite direction: an ambiguous match (two distinct containers for the
// same slug/project) is a real lookup failure and must NOT satisfy
// errors.Is(err, ErrAgentNotFound), so callers surface it as an error
// instead of treating it as "not found".
func TestLookupContainerID_AmbiguousMatchIsNotErrAgentNotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-A2",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupContainerID(context.Background(), "coordinator", "project-A")
	if err == nil {
		t.Fatal("expected an error for an ambiguous match")
	}
	if errors.Is(err, ErrAgentNotFound) {
		t.Errorf("an ambiguous match must not be classified as ErrAgentNotFound, got: %v", err)
	}
}

// TestLookupContainerID_ListingErrorIsListUnavailable proves that a
// runtime listing failure (the manager's List call itself erroring) also
// must NOT satisfy errors.Is(err, ErrAgentNotFound): it is a retryable
// infrastructure problem, not evidence the agent doesn't exist. It must
// instead satisfy errors.Is(err, ErrAgentListUnavailable), which is what
// callers such as controlchannel.go branch on.
func TestLookupContainerID_ListingErrorIsListUnavailable(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	mgr.listErr = errors.New("docker ps failed: exit status 1")
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupContainerID(context.Background(), "coordinator", "project-A")
	if err == nil {
		t.Fatal("expected an error when the runtime listing fails")
	}
	if errors.Is(err, ErrAgentNotFound) {
		t.Errorf("a listing failure must not be classified as ErrAgentNotFound, got: %v", err)
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected errors.Is(err, ErrAgentListUnavailable), got: %v", err)
	}
}

// TestLookupContainerID_AuxiliaryListErrorSurfacesUnavailable is the
// LookupContainerID analogue of
// TestLookupAgent_AuxiliaryListErrorSurfacesUnavailable (ptone/scion#2165):
// an auxiliary runtime's List failure must not be folded into "not
// found" — execCommand/resetAuth (both of which resolve their target via
// LookupContainerID) must see ErrAgentListUnavailable and respond 503, not
// 404, for the same underlying condition the PTY attach path (LookupAgent)
// already handles correctly.
func TestLookupContainerID_AuxiliaryListErrorSurfacesUnavailable(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &mockManager{listErr: errors.New("docker ps: connection refused")}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	_, err := srv.LookupContainerID(context.Background(), "ghost", "")
	if err == nil {
		t.Fatal("expected an error when the auxiliary runtime's List call fails")
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected ErrAgentListUnavailable, got: %v", err)
	}
}

// TestLookupContainerID_FallbackListErrorSurfacesUnavailable is the
// LookupContainerID analogue of
// TestLookupAgent_PrimaryManagerFallbackListErrorSurfacesUnavailable: the
// primary manager's project-less fallback List call (the backward-
// compatibility retry for unlabeled legacy containers) must propagate its
// error rather than dropping it and falling through to "not found". Before
// the fix, this fallback assigned the error to `err` and never checked it,
// so `agents` stayed nil and execution fell straight into the
// agentNotFoundError branch.
func TestLookupContainerID_FallbackListErrorSurfacesUnavailable(t *testing.T) {
	mgr := &scopedThenFailManager{failErr: errors.New("list: connection reset")}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	_, err := srv.LookupContainerID(context.Background(), "ghost", "some-project")
	if err == nil {
		t.Fatal("expected an error when the primary manager's fallback List call fails")
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected ErrAgentListUnavailable, got: %v", err)
	}
}

// TestLookupContainerID_FallbackAuxiliaryListErrorSurfacesUnavailable is the
// LookupContainerID analogue of
// TestLookupAgent_AuxiliaryListErrorInProjectFallbackSurfacesUnavailable,
// isolated to the project-scoped backward-compatibility fallback's own
// auxiliary loop (the second aux loop in LookupContainerID): the auxiliary
// runtime succeeds (empty) for the first, project-scoped aux loop, so only
// the fallback loop's List failure can be the source of the
// ErrAgentListUnavailable this test requires.
func TestLookupContainerID_FallbackAuxiliaryListErrorSurfacesUnavailable(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &scopedThenFailManager{failErr: errors.New("apiserver: context deadline exceeded")}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	_, err := srv.LookupContainerID(context.Background(), "ghost", "some-project")
	if err == nil {
		t.Fatal("expected an error when the fallback aux runtime's List call fails")
	}
	if !errors.Is(err, ErrAgentListUnavailable) {
		t.Errorf("expected ErrAgentListUnavailable, got: %v", err)
	}
}

// TestLookupAgent_PhaseIsRawRuntimeNotMerged proves that AgentLookupResult's
// Phase comes from the runtime's own listing (rawRuntimePhase), not from
// agent.Manager's merged view (which overlays agent-info.json and can lag
// behind the container's actual state — see pkg/agent/list.go). This is the
// signal classifyAttachEnd's containerRunningState relies on to decide
// whether a stopped container gets a terminal 4410 (ptone/scion#2088): if
// LookupAgent ever regressed to returning the merged phase instead, a
// container that has already restarted (or is still starting up) while its
// on-disk agent-info.json still says "stopped" would wrongly end an attach
// with a terminal code instead of retrying.
func TestLookupAgent_PhaseIsRawRuntimeNotMerged(t *testing.T) {
	tests := []struct {
		name        string
		mergedPhase string          // agent.Manager.List's overlaid phase (what ag.Phase would be)
		rawPhase    string          // the runtime's own List phase for the same container (single-entry cases)
		rawEntries  []api.AgentInfo // explicit raw List result, for cases needing more than one entry
		rawListErr  error
		wantPhase   string
	}{
		{
			name:        "raw runtime disagrees with a stale merged phase: raw wins",
			mergedPhase: "stopped",
			rawPhase:    "running",
			wantPhase:   "running",
		},
		{
			name:        "raw runtime agrees with the merged phase",
			mergedPhase: "stopped",
			rawPhase:    "stopped",
			wantPhase:   "stopped",
		},
		{
			name:        "the raw runtime list itself fails: unknown, never falls back to the merged value",
			mergedPhase: "stopped",
			rawListErr:  errors.New("docker ps failed: exit status 1"),
			wantPhase:   "",
		},
		{
			// The broader name-only filter rawRuntimePhase uses can return
			// another container that happens to share the "scion.name"
			// label (e.g. a same-named agent in a different project). The
			// container-ID match, not list order, must decide which entry's
			// phase gets used.
			name: "raw runtime lists a same-named container first: the ID match wins, not list order",
			rawEntries: []api.AgentInfo{
				{ContainerID: "other-project-container", Name: "agent1", Labels: map[string]string{"scion.name": "agent1"}, Phase: "stopped"},
				{ContainerID: "container-1", Name: "agent1", Labels: map[string]string{"scion.name": "agent1"}, Phase: "running"},
			},
			wantPhase: "running",
		},
		{
			name: "raw runtime lists entries but none match this container ID: unknown",
			rawEntries: []api.AgentInfo{
				{ContainerID: "some-other-container", Name: "agent1", Labels: map[string]string{"scion.name": "agent1"}, Phase: "stopped"},
			},
			wantPhase: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			mgr.agents = []api.AgentInfo{
				{
					ContainerID: "container-1",
					Name:        "agent1",
					Labels:      map[string]string{"scion.name": "agent1"},
					Phase:       tc.mergedPhase,
				},
			}
			rt := &runtime.MockRuntime{
				NameFunc: func() string { return "docker" },
				ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
					if tc.rawListErr != nil {
						return nil, tc.rawListErr
					}
					if tc.rawEntries != nil {
						return tc.rawEntries, nil
					}
					return []api.AgentInfo{
						{
							ContainerID: "container-1",
							Name:        "agent1",
							Labels:      map[string]string{"scion.name": "agent1"},
							Phase:       tc.rawPhase,
						},
					}, nil
				},
			}
			srv := New(DefaultServerConfig(), mgr, rt)

			result, err := srv.LookupAgent(context.Background(), "agent1", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", result.Phase, tc.wantPhase)
			}
		})
	}
}
