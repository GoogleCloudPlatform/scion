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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// TestDispatchConduitCapabilityEnv: SCION_HUB_EXPERIMENTS reaches the
// agent on create, start and restart with only the allow-listed entries of
// the dispatch set; it is absent when none is on, so sciontool never calls
// /api/v1/conduit. The broker still receives the whole set.
func TestDispatchConduitCapabilityEnv(t *testing.T) {
	paths := []struct {
		name     string
		dispatch func(context.Context, *HTTPAgentDispatcher, *store.Agent) error
		env      func(*mockRuntimeBrokerClient) map[string]string
		broker   func(*mockRuntimeBrokerClient) *RemoteHubAgentDefaults
	}{
		{"create", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreate(ctx, a)
			return err
		}, func(m *mockRuntimeBrokerClient) map[string]string { return m.lastCreateReq.ResolvedEnv },
			func(m *mockRuntimeBrokerClient) *RemoteHubAgentDefaults {
				return m.lastCreateReq.Config.HubAgentDefaults
			}},
		{"start", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentStart(ctx, a, "", false)
		}, func(m *mockRuntimeBrokerClient) map[string]string { return m.lastResolvedEnv },
			func(m *mockRuntimeBrokerClient) *RemoteHubAgentDefaults { return m.lastStartExtras.HubAgentDefaults }},
		{"restart", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentRestart(ctx, a)
		}, func(m *mockRuntimeBrokerClient) map[string]string { return m.lastRestartResolvedEnv },
			func(m *mockRuntimeBrokerClient) *RemoteHubAgentDefaults { return m.lastRestartExtras.HubAgentDefaults }},
	}
	sets := []struct {
		name    string
		set     []string
		noProv  bool
		wantEnv string // "" = absent
	}{
		{name: "no provider", noProv: true},
		{name: "none on"},
		{name: "only a broker experiment", set: []string{experiments.K8sNFSHome}},
		{name: "conduit", set: []string{conduitExperiment}, wantEnv: conduitExperiment},
		{name: "conduit and a broker experiment", set: []string{experiments.K8sNFSHome, conduitExperiment}, wantEnv: conduitExperiment},
	}
	for _, p := range paths {
		for _, c := range sets {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				ctx := context.Background()
				memStore := createTestStore(t)
				id := "conduit-env-" + p.name
				if err := memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
					ID: tid("broker-" + id), Name: "b", Slug: "b",
					Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
				}); err != nil {
					t.Fatal(err)
				}
				mock := &mockRuntimeBrokerClient{}
				d := NewHTTPAgentDispatcherWithClient(memStore, mock, false, slog.Default())
				if !c.noProv {
					d.SetDispatchExperimentsProvider(func() []string { return c.set })
				}
				agent := &store.Agent{
					ID: tid("agent-" + id), Name: id, Slug: id,
					ProjectID: tid("project-" + id), OwnerID: tid("user-" + id),
					RuntimeBrokerID: tid("broker-" + id),
					AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
				}
				if err := p.dispatch(ctx, d, agent); err != nil {
					t.Fatalf("dispatch: %v", err)
				}
				got, ok := p.env(mock)[envHubExperiments]
				if c.wantEnv == "" {
					assert.False(t, ok, "%s = %q, want absent", envHubExperiments, got)
				} else {
					assert.Equal(t, c.wantEnv, got, envHubExperiments)
				}
				var brokerSet []string
				if hd := p.broker(mock); hd != nil {
					brokerSet = hd.Experiments
				}
				assert.Equal(t, c.set, brokerSet, "broker experiments")
			})
		}
	}
}

// TestApplyAgentExperimentsOwnsTheVariable: a value from config or storage
// env never survives; the hub sets the variable to the allow-listed
// entries or removes it.
func TestApplyAgentExperimentsOwnsTheVariable(t *testing.T) {
	for _, tt := range []struct {
		name       string
		in         string
		dispatched []string
		want       string // "" = removed
	}{
		{name: "forged value removed", in: conduitExperiment},
		{name: "forged value replaced", in: "x.y", dispatched: []string{conduitExperiment}, want: conduitExperiment},
		{name: "broker experiment not sent", in: conduitExperiment, dispatched: []string{experiments.K8sNFSHome}},
		{name: "unknown name not sent", dispatched: []string{"hub.other", conduitExperiment}, want: conduitExperiment},
		{name: "duplicate sent once", dispatched: []string{conduitExperiment, conduitExperiment}, want: conduitExperiment},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			cls := map[string]api.EnvKind{}
			if tt.in != "" {
				env[envHubExperiments] = tt.in
				cls[envHubExperiments] = api.EnvKindPlain
			}
			applyAgentExperiments(env, &cls, tt.dispatched)
			got, ok := env[envHubExperiments]
			_, classified := cls[envHubExperiments]
			if tt.want == "" {
				assert.False(t, ok || classified, "env %q (present %v) classified %v, want removed", got, ok, classified)
				return
			}
			assert.Equal(t, tt.want, got)
			assert.True(t, classified, "classified")
		})
	}
}
