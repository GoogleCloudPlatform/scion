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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDispatchAgentProvision_StoresBrokerReportedRuntime: the runtime a
// broker reports for a provision-only create (runtimebroker's AgentResponse
// RuntimeType) is what the hub records as the agent's runtime, for an agent
// provisioned on a kubernetes profile of a docker-default broker and for a
// docker agent on the same broker (ptone/scion#2633).
func TestDispatchAgentProvision_StoresBrokerReportedRuntime(t *testing.T) {
	for _, reported := range []string{"kubernetes", "docker"} {
		t.Run(reported, func(t *testing.T) {
			ctx := context.Background()
			var provisionOnly bool
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req RemoteCreateAgentRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
				}
				provisionOnly = req.ProvisionOnly
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(runtimebroker.CreateAgentResponse{
					Agent: &runtimebroker.AgentResponse{
						ID:          req.ID,
						Slug:        req.Slug,
						Name:        req.Name,
						Status:      string(state.PhaseCreated),
						Phase:       string(state.PhaseCreated),
						RuntimeType: reported,
					},
					Created: true,
				})
			}))
			defer broker.Close()

			memStore := createTestStore(t)
			if err := memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID:       tid("host-1"),
				Name:     "test-host",
				Slug:     "test-host",
				Endpoint: broker.URL,
				Status:   store.BrokerStatusOnline,
			}); err != nil {
				t.Fatalf("create runtime broker: %v", err)
			}
			dispatcher := NewHTTPAgentDispatcherWithClient(memStore, NewHTTPRuntimeBrokerClient(), false, slog.Default())

			agent := &store.Agent{
				ID:              tid("agent-1"),
				Name:            "test-agent",
				Slug:            "test-agent",
				ProjectID:       tid("project-1"),
				RuntimeBrokerID: tid("host-1"),
				AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
			}
			if err := dispatcher.DispatchAgentProvision(ctx, agent); err != nil {
				t.Fatalf("DispatchAgentProvision: %v", err)
			}
			if !provisionOnly {
				t.Fatal("broker did not receive a provision-only request")
			}
			if agent.Runtime != reported {
				t.Errorf("agent.Runtime = %q, want %q", agent.Runtime, reported)
			}
		})
	}
}
