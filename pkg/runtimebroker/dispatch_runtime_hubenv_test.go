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
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// These tests pin that the hub endpoint's container-bridge rewrite, the
// colocated extra hosts, and the host-side worktree provisioning check all
// follow the runtime a dispatch resolved to (its profile), not the broker's
// default runtime (ptone/scion#2623).

const (
	dispatchRuntimeOtherProfile = "other-runtime"
	dispatchRuntimeBridge       = "http://host.docker.internal:9090"
)

type dispatchRuntimeCase struct {
	name           string
	defaultRuntime string
	profileRuntime string
	// useProfile selects dispatchRuntimeOtherProfile (resolving to
	// profileRuntime) instead of the active profile (defaultRuntime).
	useProfile bool
	// wantContainerRouting reports whether the agent is expected to get the
	// container-bridge rewrite and the colocated --add-host entries.
	wantContainerRouting bool
}

var dispatchRuntimeCases = []dispatchRuntimeCase{
	{name: "docker default, docker agent", defaultRuntime: "docker", profileRuntime: "kubernetes", wantContainerRouting: true},
	{name: "docker default, kubernetes agent", defaultRuntime: "docker", profileRuntime: "kubernetes", useProfile: true, wantContainerRouting: false},
	{name: "kubernetes default, kubernetes agent", defaultRuntime: "kubernetes", profileRuntime: "docker", wantContainerRouting: false},
	{name: "kubernetes default, docker agent", defaultRuntime: "kubernetes", profileRuntime: "docker", useProfile: true, wantContainerRouting: true},
}

// newDispatchRuntimeServer builds a broker whose default runtime is
// tc.defaultRuntime, with a second profile resolving to tc.profileRuntime,
// a container hub endpoint pointing at the docker bridge, and a colocated hub
// connection.
func newDispatchRuntimeServer(t *testing.T, tc dispatchRuntimeCase, containerHubEndpoint string) (*Server, string) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.ContainerHubEndpoint = containerHubEndpoint
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tc.defaultRuntime, dispatchRuntimeOtherProfile, tc.profileRuntime)
	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:        "hub-1",
		IsColocated: true,
		Hydrator:    newTestHydrator(t),
	}
	srv.hubMu.Unlock()
	return srv, dotScion
}

func (tc dispatchRuntimeCase) profile() string {
	if tc.useProfile {
		return dispatchRuntimeOtherProfile
	}
	return ""
}

func buildDispatchRuntimeStartContext(t *testing.T, srv *Server, tc dispatchRuntimeCase, hubEndpoint string) *startContext {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	r.Header.Set("X-Scion-Hub-Connection", "hub-1")
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-dispatch-runtime",
		HubEndpoint: hubEndpoint,
		Config:      &CreateAgentConfig{Profile: tc.profile()},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	return sc
}

// TestBuildStartContext_LocalhostHubEndpointFollowsDispatchRuntime: with a
// localhost hub endpoint and a docker-bridge container hub endpoint, only an
// agent dispatched to a container runtime gets the bridge rewrite (and the
// host-gateway mapping for the bridge hostname). An agent dispatched to
// kubernetes keeps the endpoint as resolved and gets no extra hosts, whatever
// the broker's default runtime is.
func TestBuildStartContext_LocalhostHubEndpointFollowsDispatchRuntime(t *testing.T) {
	clearSCIONEnv(t)
	for _, tc := range dispatchRuntimeCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newDispatchRuntimeServer(t, tc, dispatchRuntimeBridge)
			sc := buildDispatchRuntimeStartContext(t, srv, tc, "http://localhost:8080")

			wantEndpoint := "http://localhost:8080"
			var wantHosts []string
			if tc.wantContainerRouting {
				wantEndpoint = "http://host.docker.internal:8080"
				wantHosts = []string{"host.docker.internal:host-gateway"}
			}
			if got := sc.Opts.Env["SCION_HUB_ENDPOINT"]; got != wantEndpoint {
				t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", got, wantEndpoint)
			}
			if got := sc.Opts.Env["SCION_HUB_URL"]; got != wantEndpoint {
				t.Errorf("SCION_HUB_URL = %q, want %q", got, wantEndpoint)
			}
			if !slices.Equal(sc.Opts.ExtraHosts, wantHosts) {
				t.Errorf("ExtraHosts = %v, want %v", sc.Opts.ExtraHosts, wantHosts)
			}
		})
	}
}

// TestBuildStartContext_ColocatedExtraHostsFollowDispatchRuntime: with a
// colocated hub on a public domain (no bridge rewrite involved), only an
// agent dispatched to a container runtime gets the domain mapped to
// host-gateway.
func TestBuildStartContext_ColocatedExtraHostsFollowDispatchRuntime(t *testing.T) {
	clearSCIONEnv(t)
	const publicEndpoint = "https://hub.example.com"
	for _, tc := range dispatchRuntimeCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newDispatchRuntimeServer(t, tc, "")
			sc := buildDispatchRuntimeStartContext(t, srv, tc, publicEndpoint)

			var wantHosts []string
			if tc.wantContainerRouting {
				wantHosts = []string{"hub.example.com:host-gateway"}
			}
			if got := sc.Opts.Env["SCION_HUB_ENDPOINT"]; got != publicEndpoint {
				t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", got, publicEndpoint)
			}
			if !slices.Equal(sc.Opts.ExtraHosts, wantHosts) {
				t.Errorf("ExtraHosts = %v, want %v", sc.Opts.ExtraHosts, wantHosts)
			}
		})
	}
}

// TestBuildStartContext_WorktreeProvisionFollowsDispatchRuntime: host-side
// worktree-per-agent provisioning runs only for an agent dispatched to a
// container runtime. An agent dispatched to kubernetes falls back to the
// in-container clone (no host worktree mounted), whatever the broker's
// default runtime is.
func TestBuildStartContext_WorktreeProvisionFollowsDispatchRuntime(t *testing.T) {
	requireWorktreeGit(t)
	clearSCIONEnv(t)
	for _, tc := range dispatchRuntimeCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, dotScion := newDispatchRuntimeServer(t, tc, "")
			bare := initBareRepoWithCommit(t)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "agent-a",
				AgentID:       "agent-a",
				ProjectID:     "p1",
				ProjectSlug:   "proj",
				ProjectPath:   dotScion,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config: &CreateAgentConfig{
					Profile:  tc.profile(),
					GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"},
				},
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if tc.wantContainerRouting {
				if sc.Opts.Workspace == "" {
					t.Error("expected a host worktree Workspace for a container-runtime agent, got none")
				}
				if got := sc.Opts.Env["SCION_GIT_CLONE_URL"]; got != "" {
					t.Errorf("expected no in-container clone for a host worktree, got SCION_GIT_CLONE_URL=%q", got)
				}
				return
			}
			if sc.Opts.Workspace != "" {
				t.Errorf("expected no host worktree for a kubernetes agent, got Workspace=%q", sc.Opts.Workspace)
			}
			if got := sc.Opts.Env["SCION_GIT_CLONE_URL"]; got != bare {
				t.Errorf("expected the in-container clone fallback, got SCION_GIT_CLONE_URL=%q", got)
			}
		})
	}
}

// TestBuildStartContext_CloudrunSandboxEndpointFollowsDispatchRuntime: on a
// broker whose default runtime is cloudrun-sandbox, only an agent dispatched
// to cloudrun-sandbox gets the link-local sandbox hub endpoint. An agent
// dispatched to a kubernetes profile on the same broker keeps the hub
// endpoint as resolved.
func TestBuildStartContext_CloudrunSandboxEndpointFollowsDispatchRuntime(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("SCION_METADATA_BIND_ADDRESS", "203.0.113.5")
	tests := []struct {
		name    string
		profile string
		want    string
	}{
		{name: "cloudrun-sandbox agent", profile: "", want: "http://203.0.113.5:8080"},
		{name: "kubernetes agent", profile: dispatchRuntimeOtherProfile, want: "http://localhost:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			cfg.HubListenPort = 8080
			srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "cloudrun-sandbox", dispatchRuntimeOtherProfile, "kubernetes")
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-cloudrun-dispatch",
				HubEndpoint: "http://localhost:8080",
				Config:      &CreateAgentConfig{Profile: tt.profile},
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if got := sc.Opts.Env["SCION_HUB_ENDPOINT"]; got != tt.want {
				t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildStartContext_EmptyPerAgentFollowsNoDispatchRuntime: the
// empty-per-agent mode (design #2703) is runtime-independent in the start
// context: whichever runtime the dispatch resolves to (docker, or kubernetes
// with local storage), the agent gets the mode, no SCION_WORKSPACE_GIT, no
// host worktree, no in-container clone, and the private-workspace flag for
// ProvisionAgent.
func TestBuildStartContext_EmptyPerAgentFollowsNoDispatchRuntime(t *testing.T) {
	clearSCIONEnv(t)
	for _, tc := range dispatchRuntimeCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, dotScion := newDispatchRuntimeServer(t, tc, "")
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "agent-a",
				AgentID:       "agent-a",
				ProjectID:     "p1",
				ProjectSlug:   "proj",
				ProjectPath:   dotScion,
				WorkspaceMode: "empty-per-agent",
				Config:        &CreateAgentConfig{Profile: tc.profile()},
				HTTPRequest:   httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:     opCreate,
			})
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if got := sc.Opts.Env["SCION_WORKSPACE_MODE"]; got != string(store.SharingModeEmptyPerAgent) {
				t.Errorf("SCION_WORKSPACE_MODE = %q, want %q", got, store.SharingModeEmptyPerAgent)
			}
			for _, k := range []string{"SCION_WORKSPACE_GIT", "SCION_GIT_CLONE_URL"} {
				if got, ok := sc.Opts.Env[k]; ok {
					t.Errorf("%s = %q, want unset", k, got)
				}
			}
			if sc.Opts.Workspace != "" {
				t.Errorf("Workspace = %q, want empty (ProvisionAgent picks agents/<slug>/workspace)", sc.Opts.Workspace)
			}
			if !sc.Opts.EmptyPerAgentWorkspace {
				t.Error("EmptyPerAgentWorkspace = false, want true")
			}
		})
	}
}
