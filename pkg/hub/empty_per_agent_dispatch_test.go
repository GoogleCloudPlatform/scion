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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Dispatcher-side tests for the empty-per-agent workspace mode (design #2703
// §2.3 / D3): wire value, env, and the fail-closed capability gate.

type emptyPerAgentFixture struct {
	store      store.Store
	client     *mockRuntimeBrokerClient
	dispatcher *HTTPAgentDispatcher
	agent      *store.Agent
}

func newEmptyPerAgentFixture(t *testing.T, name string, capable bool) *emptyPerAgentFixture {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{
		ID:     tid("proj-epa-" + name),
		Name:   "empty-per-agent",
		Slug:   "empty-per-agent",
		Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent},
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	broker := &store.RuntimeBroker{
		ID:       tid("broker-epa-" + name),
		Name:     "epa-broker",
		Slug:     "epa-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if capable {
		broker.Capabilities = &store.BrokerCapabilities{EmptyPerAgentWorkspace: true}
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("create broker: %v", err)
	}
	client := &mockRuntimeBrokerClient{}
	return &emptyPerAgentFixture{
		store:      memStore,
		client:     client,
		dispatcher: NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default()),
		agent: &store.Agent{
			ID:              tid("agent-epa-" + name),
			Name:            "epa-agent",
			Slug:            "epa-agent",
			ProjectID:       project.ID,
			RuntimeBrokerID: broker.ID,
			AppliedConfig:   &store.AgentAppliedConfig{},
		},
	}
}

func TestEmptyPerAgent_DispatchCreate_SendsCanonicalMode(t *testing.T) {
	f := newEmptyPerAgentFixture(t, "create", true)
	if err := f.dispatcher.DispatchAgentCreate(context.Background(), f.agent); err != nil {
		t.Fatalf("DispatchAgentCreate: %v", err)
	}
	req := f.client.lastCreateReq
	if req == nil {
		t.Fatal("expected CreateAgent to be called")
	}
	if req.WorkspaceMode != string(store.SharingModeEmptyPerAgent) {
		t.Errorf("wire WorkspaceMode = %q, want %q (never the bare per-agent label)", req.WorkspaceMode, store.SharingModeEmptyPerAgent)
	}
	if req.Config != nil && req.Config.Workspace != "" {
		t.Errorf("Config.Workspace = %q, want empty", req.Config.Workspace)
	}
	if req.Config != nil && req.Config.GitClone != nil {
		t.Errorf("Config.GitClone = %+v, want nil", req.Config.GitClone)
	}
}

func TestEmptyPerAgent_DispatchStartRestart_EnvAndSpec(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "start", true)
		if err := f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false); err != nil {
			t.Fatalf("DispatchAgentStart: %v", err)
		}
		assertEmptyPerAgentEnv(t, f.client.lastResolvedEnv)
		if got := f.client.lastStartExtras.Workspace.WorkspaceMode; got != string(store.SharingModeEmptyPerAgent) {
			t.Errorf("start WorkspaceDispatchSpec.WorkspaceMode = %q, want %q", got, store.SharingModeEmptyPerAgent)
		}
	})
	t.Run("restart", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "restart", true)
		if err := f.dispatcher.DispatchAgentRestart(context.Background(), f.agent); err != nil {
			t.Fatalf("DispatchAgentRestart: %v", err)
		}
		// Restart carries the mode via resolvedEnv only (no workspace spec).
		assertEmptyPerAgentEnv(t, f.client.lastRestartResolvedEnv)
	})
}

func assertEmptyPerAgentEnv(t *testing.T, env map[string]string) {
	t.Helper()
	if got := env["SCION_WORKSPACE_MODE"]; got != string(store.SharingModeEmptyPerAgent) {
		t.Errorf("SCION_WORKSPACE_MODE = %q, want %q", got, store.SharingModeEmptyPerAgent)
	}
	if v, ok := env["SCION_WORKSPACE_GIT"]; ok {
		t.Errorf("SCION_WORKSPACE_GIT must be absent for empty-per-agent, got %q", v)
	}
}

// TestEmptyPerAgent_DispatchFailsClosedWithoutCapability: every provisioning
// dispatch refuses a broker that does not advertise emptyPerAgentWorkspace,
// without calling the broker.
func TestEmptyPerAgent_DispatchFailsClosedWithoutCapability(t *testing.T) {
	ops := []struct {
		name string
		run  func(f *emptyPerAgentFixture) error
	}{
		{"create", func(f *emptyPerAgentFixture) error {
			return f.dispatcher.DispatchAgentCreate(context.Background(), f.agent)
		}},
		{"create-with-gather", func(f *emptyPerAgentFixture) error {
			_, err := f.dispatcher.DispatchAgentCreateWithGather(context.Background(), f.agent)
			return err
		}},
		{"provision", func(f *emptyPerAgentFixture) error {
			return f.dispatcher.DispatchAgentProvision(context.Background(), f.agent)
		}},
		{"start", func(f *emptyPerAgentFixture) error {
			return f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false)
		}},
		{"restart", func(f *emptyPerAgentFixture) error {
			return f.dispatcher.DispatchAgentRestart(context.Background(), f.agent)
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			f := newEmptyPerAgentFixture(t, "nocap-"+op.name, false)
			err := op.run(f)
			if !errors.Is(err, errBrokerLacksEmptyPerAgent) {
				t.Fatalf("err = %v, want errBrokerLacksEmptyPerAgent", err)
			}
			if f.client.createCalled || f.client.startCalled || f.client.restartCalled {
				t.Error("broker must not be called when the capability is missing")
			}
		})
	}

	t.Run("stop is not gated", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "nocap-stop", false)
		if err := f.dispatcher.DispatchAgentStop(context.Background(), f.agent); err != nil {
			t.Fatalf("DispatchAgentStop: %v", err)
		}
	})
}
