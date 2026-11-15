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
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestEphemeralPersistenceRefusal: an ephemeral dispatch is refused unless
// the hub forwarded hub.cloudrun_ephemeral_agents (in ctx on start and
// restart, in the create config on create); a persistent one never is.
func TestEphemeralPersistenceRefusal(t *testing.T) {
	gate := &api.HubAgentDefaults{Experiments: []string{experiments.CloudRunEphemeralAgents}}
	gateCtx := api.ContextWithHubAgentDefaults(context.Background(), gate)
	bg := context.Background()
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		in         startContextInputs
		wantStatus int
	}{
		{name: "persistent, gate off", ctx: bg, in: startContextInputs{Name: "a"}},
		{name: "explicit persistent, gate off", ctx: bg, in: startContextInputs{Name: "a", Persistence: api.PersistencePersistent}},
		{name: "ephemeral, gate off", ctx: bg, in: startContextInputs{Name: "a", Persistence: api.PersistenceEphemeral}, wantStatus: http.StatusPreconditionFailed},
		{name: "ephemeral, other experiment only", ctx: api.ContextWithHubAgentDefaults(bg, &api.HubAgentDefaults{Experiments: []string{experiments.CloudRunNativeIdentity}}),
			in: startContextInputs{Name: "a", Persistence: api.PersistenceEphemeral}, wantStatus: http.StatusPreconditionFailed},
		{name: "ephemeral start, gate in ctx", ctx: gateCtx, in: startContextInputs{Name: "a", Persistence: api.PersistenceEphemeral}},
		{name: "ephemeral create, gate in config", ctx: bg, in: startContextInputs{Name: "a", Persistence: api.PersistenceEphemeral, Config: &CreateAgentConfig{HubAgentDefaults: gate}}},
		{name: "unknown value", ctx: gateCtx, in: startContextInputs{Name: "a", Persistence: "sometimes"}, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sce := ephemeralPersistenceRefusal(tc.ctx, tc.in)
			if tc.wantStatus == 0 {
				if sce != nil {
					t.Fatalf("refused: %v", sce)
				}
				return
			}
			if sce == nil || sce.Status != tc.wantStatus {
				t.Fatalf("refusal = %+v, want status %d", sce, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusPreconditionFailed && sce.Code != "experiment_disabled" {
				t.Errorf("code = %q, want experiment_disabled", sce.Code)
			}
		})
	}
}

// TestStaticCapabilities_EphemeralPersistence: no broker advertises
// ephemeralPersistence until its runtime implements it (Cloud Run, in a
// follow-up), so the hub never accepts an ephemeral create a Run refuses.
func TestStaticCapabilities_EphemeralPersistence(t *testing.T) {
	for _, rt := range []scionrt.Runtime{nil, &scionrt.CloudRunRuntime{}, &scionrt.DockerRuntime{}, &scionrt.KubernetesRuntime{}} {
		if StaticCapabilities(rt, "").EphemeralPersistence || slices.Contains(StaticCapabilityNames(rt), "ephemeralPersistence") {
			t.Errorf("%T advertises ephemeralPersistence", rt)
		}
	}
}

// TestPersistenceRuntimeRefusal: the resolved runtime decides, with a 422
// persistence_unsupported; persistent passes on any runtime.
func TestPersistenceRuntimeRefusal(t *testing.T) {
	sce := persistenceRuntimeRefusal("a", api.PersistenceEphemeral, &scionrt.DockerRuntime{})
	if sce == nil || sce.Status != http.StatusUnprocessableEntity || sce.Code != "persistence_unsupported" {
		t.Fatalf("docker ephemeral = %+v, want 422 persistence_unsupported", sce)
	}
	if sce := persistenceRuntimeRefusal("a", api.PersistenceEphemeral, nil); sce == nil {
		t.Fatal("nil runtime ephemeral: want a refusal")
	}
	if sce := persistenceRuntimeRefusal("a", "", &scionrt.DockerRuntime{}); sce != nil {
		t.Fatalf("persistent: %+v", sce)
	}
}

// ephemeralBrokerRuntime is a mock runtime with ephemeral support.
type ephemeralBrokerRuntime struct{ *scionrt.MockRuntime }

func (ephemeralBrokerRuntime) SupportsEphemeralPersistence() bool { return true }

// newEphemeralTestServer is newTestServerWithManager with a runtime that
// supports ephemeral persistence when capable is set.
func newEphemeralTestServer(t *testing.T, capable bool) (*Server, *mockManager) {
	t.Helper()
	if !capable {
		srv := newTestServer(t)
		return srv, srv.manager.(*mockManager)
	}
	setupTestScionEnv(t)
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "running", ContainerStatus: "Up 1 hour"}}}
	srv := New(cfg, mgr, ephemeralBrokerRuntime{&scionrt.MockRuntime{NameFunc: func() string { return "mock" }}}, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	return srv, mgr
}

// TestEphemeralPersistence_Handlers: create, start and restart take the
// persistence from the request: with the experiment missing from the
// forwarded hub defaults they answer 412 experiment_disabled, on a runtime
// without ephemeral support 422 persistence_unsupported, both before Start;
// on a supporting runtime Start receives it.
func TestEphemeralPersistence_Handlers(t *testing.T) {
	gate := `{"experiments": ["` + experiments.CloudRunEphemeralAgents + `"]}`
	bodies := map[string]func(gateOn bool) string{
		"/api/v1/agents": func(gateOn bool) string {
			cfg := `"template": "claude"`
			if gateOn {
				cfg += `, "hubAgentDefaults": ` + gate
			}
			return `{"name": "eph-agent", "persistence": "ephemeral", "config": {` + cfg + `}}`
		},
		"/api/v1/agents/test-agent-1/start": func(gateOn bool) string {
			if gateOn {
				return `{"persistence": "ephemeral", "hubAgentDefaults": ` + gate + `}`
			}
			return `{"persistence": "ephemeral"}`
		},
		"/api/v1/agents/test-agent-1/restart": func(gateOn bool) string {
			if gateOn {
				return `{"persistence": "ephemeral", "hubAgentDefaults": ` + gate + `}`
			}
			return `{"persistence": "ephemeral"}`
		},
	}
	post := func(srv *Server, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	for path, body := range bodies {
		t.Run(path+"/gate off", func(t *testing.T) {
			srv, mgr := newEphemeralTestServer(t, true)
			w := post(srv, path, body(false))
			if w.Code != http.StatusPreconditionFailed || !strings.Contains(w.Body.String(), "experiment_disabled") {
				t.Fatalf("got %d %s, want 412 experiment_disabled", w.Code, w.Body.String())
			}
			if mgr.startCalls != 0 {
				t.Errorf("Start called %d times", mgr.startCalls)
			}
		})
		t.Run(path+"/runtime without support", func(t *testing.T) {
			srv, mgr := newEphemeralTestServer(t, false)
			w := post(srv, path, body(true))
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "persistence_unsupported") {
				t.Fatalf("got %d %s, want 422 persistence_unsupported", w.Code, w.Body.String())
			}
			if mgr.startCalls != 0 {
				t.Errorf("Start called %d times", mgr.startCalls)
			}
		})
		t.Run(path+"/supported", func(t *testing.T) {
			srv, mgr := newEphemeralTestServer(t, true)
			w := post(srv, path, body(true))
			if w.Code >= 300 {
				t.Fatalf("got %d %s, want success", w.Code, w.Body.String())
			}
			if mgr.lastStartOpts.Persistence != api.PersistenceEphemeral {
				t.Errorf("Start persistence = %q, want ephemeral", mgr.lastStartOpts.Persistence)
			}
		})
	}
}
