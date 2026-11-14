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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
