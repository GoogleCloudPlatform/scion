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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
	return newWorkspaceModeDispatchFixture(t, name, "", store.WorkspaceModePerAgent, capable)
}

// newWorkspaceModeDispatchFixture builds a dispatcher fixture for a project
// with the given git remote and raw workspace-mode label.
func newWorkspaceModeDispatchFixture(t *testing.T, name, gitRemote, label string, capable bool) *emptyPerAgentFixture {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{
		ID:        tid("proj-epa-" + name),
		Name:      "empty-per-agent",
		Slug:      "empty-per-agent",
		GitRemote: gitRemote,
	}
	if label != "" {
		project.Labels = map[string]string{store.LabelWorkspaceMode: label}
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
		{"finalize-env", func(f *emptyPerAgentFixture) error {
			return f.dispatcher.DispatchFinalizeEnv(context.Background(), f.agent, map[string]string{"K": "v"})
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

// projectLookupErrorStore makes GetProject fail with a non-NotFound error, to
// prove the capability gate fails closed on a transient lookup error.
type projectLookupErrorStore struct {
	store.Store
	err error
}

func (s *projectLookupErrorStore) GetProject(context.Context, string) (*store.Project, error) {
	return nil, s.err
}

// TestEmptyPerAgent_GateFailsClosedOnProjectLookupError: a project lookup
// error must abort the provisioning dispatch (design #2703 D3), while a
// deleted project (ErrNotFound) has nothing to gate.
func TestEmptyPerAgent_GateFailsClosedOnProjectLookupError(t *testing.T) {
	t.Run("transient error fails closed", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "lookup-err", false)
		failing := &projectLookupErrorStore{Store: f.store, err: errors.New("db unavailable")}
		d := NewHTTPAgentDispatcherWithClient(failing, f.client, false, slog.Default())
		_, err := d.getProvisioningBrokerEndpoint(context.Background(), f.agent)
		if err == nil || !strings.Contains(err.Error(), "failed to load project for capability check") {
			t.Fatalf("err = %v, want project lookup failure", err)
		}
	})
	t.Run("not found skips the check", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "lookup-nf", false)
		missing := &projectLookupErrorStore{Store: f.store, err: store.ErrNotFound}
		d := NewHTTPAgentDispatcherWithClient(missing, f.client, false, slog.Default())
		endpoint, err := d.getProvisioningBrokerEndpoint(context.Background(), f.agent)
		if err != nil {
			t.Fatalf("err = %v, want nil for a deleted project", err)
		}
		if endpoint == "" {
			t.Error("expected the broker endpoint")
		}
	})
}

// flakyProjectStore lets the first okCalls GetProject calls through (so the
// capability gate passes) and fails every later one with err.
type flakyProjectStore struct {
	store.Store
	okCalls int
	calls   int
	err     error
}

func (s *flakyProjectStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	s.calls++
	if s.calls <= s.okCalls {
		return s.Store.GetProject(ctx, id)
	}
	return nil, s.err
}

// TestEmptyPerAgent_DispatchInfoFailsClosedOnProjectLookupError: a project
// lookup error after the capability gate must fail the dispatch instead of
// sending an empty workspace mode to a capable broker (review #2717 r3 N1).
// A deleted project still dispatches with empty info, and delete stays
// best-effort.
func TestEmptyPerAgent_DispatchInfoFailsClosedOnProjectLookupError(t *testing.T) {
	ops := []struct {
		name string
		run  func(d *HTTPAgentDispatcher, a *store.Agent) error
	}{
		{"create", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentCreate(context.Background(), a)
		}},
		{"start", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentStart(context.Background(), a, "", false)
		}},
		{"restart", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentRestart(context.Background(), a)
		}},
	}
	for _, op := range ops {
		t.Run(op.name+" transient error fails closed", func(t *testing.T) {
			f := newEmptyPerAgentFixture(t, "info-err-"+op.name, true)
			flaky := &flakyProjectStore{Store: f.store, okCalls: 1, err: errors.New("db unavailable")}
			d := NewHTTPAgentDispatcherWithClient(flaky, f.client, false, slog.Default())
			err := op.run(d, f.agent)
			if err == nil || !strings.Contains(err.Error(), "resolve project") {
				t.Fatalf("err = %v, want project resolve failure", err)
			}
			if f.client.createCalled || f.client.startCalled || f.client.restartCalled {
				t.Error("broker must not be called when the project lookup fails")
			}
		})
	}

	t.Run("resolve: not found yields empty info", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "info-nf", true)
		d := NewHTTPAgentDispatcherWithClient(&projectLookupErrorStore{Store: f.store, err: store.ErrNotFound}, f.client, false, slog.Default())
		info, err := d.resolveDispatchProjectInfo(context.Background(), f.agent)
		if err != nil {
			t.Fatalf("err = %v, want nil for a deleted project", err)
		}
		if info.workspaceMode != "" || info.projectSlug != "" {
			t.Errorf("info = %+v, want empty", info)
		}
	})

	t.Run("delete stays best-effort", func(t *testing.T) {
		f := newEmptyPerAgentFixture(t, "info-del", true)
		d := NewHTTPAgentDispatcherWithClient(&projectLookupErrorStore{Store: f.store, err: errors.New("db unavailable")}, f.client, false, slog.Default())
		if err := d.DispatchAgentDelete(context.Background(), f.agent, false, false, false, time.Time{}); err != nil {
			t.Fatalf("DispatchAgentDelete: %v", err)
		}
	})
}

// TestDispatchWorkspaceMode: the value sent to the broker must resolve,
// label-only, to the hub's SharingMode (review #2717 r2 finding 2).
func TestDispatchWorkspaceMode(t *testing.T) {
	const remote = "github.com/a/b"
	cases := []struct {
		name      string
		gitRemote string
		label     string
		want      string
	}{
		{"non-git unlabelled", "", "", ""},
		{"non-git shared", "", "shared", "shared"},
		{"non-git per-agent", "", "per-agent", "empty-per-agent"},
		{"non-git legacy empty-per-agent", "", "empty-per-agent", "empty-per-agent"},
		{"non-git legacy worktree dropped", "", "worktree-per-agent", ""},
		{"non-git legacy clone-per-agent dropped", "", "clone-per-agent", ""},
		{"git unlabelled", remote, "", ""},
		{"git shared", remote, "shared", "shared"},
		{"git per-agent", remote, "per-agent", "per-agent"},
		{"git worktree", remote, "worktree-per-agent", "worktree-per-agent"},
		{"git legacy clone-per-agent", remote, "clone-per-agent", "clone-per-agent"},
		{"git legacy empty-per-agent dropped", remote, "empty-per-agent", ""},
		{"git unknown forwarded", remote, "bogus", "bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &store.Project{GitRemote: tc.gitRemote}
			if tc.label != "" {
				p.Labels = map[string]string{store.LabelWorkspaceMode: tc.label}
			}
			got := dispatchWorkspaceMode(p)
			if got != tc.want {
				t.Fatalf("dispatchWorkspaceMode = %q, want %q", got, tc.want)
			}
			if rt := store.ResolveWorkspaceSharingMode(got); rt != p.SharingMode() {
				t.Errorf("broker resolution %q != hub SharingMode %q", rt, p.SharingMode())
			}
		})
	}
}

// TestDispatch_GitProjectLegacyEmptyPerAgentLabel: a git project with a raw
// legacy "empty-per-agent" label is shared-plain on the hub, so the broker
// must not receive empty-per-agent, and no capability is required.
func TestDispatch_GitProjectLegacyEmptyPerAgentLabel(t *testing.T) {
	const remote = "github.com/a/b"
	t.Run("create", func(t *testing.T) {
		f := newWorkspaceModeDispatchFixture(t, "git-legacy-create", remote, "empty-per-agent", false)
		if err := f.dispatcher.DispatchAgentCreate(context.Background(), f.agent); err != nil {
			t.Fatalf("DispatchAgentCreate (no capability needed): %v", err)
		}
		if got := f.client.lastCreateReq.WorkspaceMode; got != "" {
			t.Errorf("wire WorkspaceMode = %q, want empty", got)
		}
	})
	t.Run("start", func(t *testing.T) {
		f := newWorkspaceModeDispatchFixture(t, "git-legacy-start", remote, "empty-per-agent", false)
		f.agent.AppliedConfig.GitClone = &api.GitCloneConfig{URL: "https://github.com/a/b.git"}
		if err := f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false); err != nil {
			t.Fatalf("DispatchAgentStart (no capability needed): %v", err)
		}
		env := f.client.lastResolvedEnv
		if got, ok := env["SCION_WORKSPACE_MODE"]; ok {
			t.Errorf("SCION_WORKSPACE_MODE = %q, want absent (unlabelled shared-plain)", got)
		}
		if env["SCION_WORKSPACE_GIT"] != "true" {
			t.Errorf("SCION_WORKSPACE_GIT = %q, want true for a git shared-plain workspace", env["SCION_WORKSPACE_GIT"])
		}
		if got := f.client.lastStartExtras.Workspace.WorkspaceMode; got != "" {
			t.Errorf("start spec WorkspaceMode = %q, want empty", got)
		}
	})
}
