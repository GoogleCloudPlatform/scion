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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDispatchConduitCapabilityEnv: SCION_HUB_CONDUIT=true reaches the
// agent on create, start and restart only while the capability check
// reports true; otherwise it is absent, so sciontool never calls
// /api/v1/conduit.
func TestDispatchConduitCapabilityEnv(t *testing.T) {
	paths := []struct {
		name     string
		dispatch func(context.Context, *HTTPAgentDispatcher, *store.Agent) error
		env      func(*mockRuntimeBrokerClient) map[string]string
	}{
		{"create", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreate(ctx, a)
			return err
		}, func(m *mockRuntimeBrokerClient) map[string]string { return m.lastCreateReq.ResolvedEnv }},
		{"start", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentStart(ctx, a, "", false)
		}, func(m *mockRuntimeBrokerClient) map[string]string { return m.lastResolvedEnv }},
		{"restart", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentRestart(ctx, a)
		}, func(m *mockRuntimeBrokerClient) map[string]string { return m.lastRestartResolvedEnv }},
	}
	caps := []struct {
		name string
		fn   func() bool
		want bool
	}{
		{"unset", nil, false},
		{"off", func() bool { return false }, false},
		{"on", func() bool { return true }, true},
	}
	for _, p := range paths {
		for _, c := range caps {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				ctx := context.Background()
				memStore := createTestStore(t)
				id := "conduit-env-" + p.name + "-" + c.name
				if err := memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
					ID: tid("broker-" + id), Name: "b", Slug: "b",
					Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
				}); err != nil {
					t.Fatal(err)
				}
				mock := &mockRuntimeBrokerClient{}
				d := NewHTTPAgentDispatcherWithClient(memStore, mock, false, slog.Default())
				if c.fn != nil {
					d.SetConduitCapability(c.fn)
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
				got, ok := p.env(mock)[envHubConduit]
				if c.want && got != "true" {
					t.Fatalf("%s = %q, want true", envHubConduit, got)
				}
				if !c.want && ok {
					t.Fatalf("%s = %q, want absent", envHubConduit, got)
				}
			})
		}
	}
}

// TestApplyConduitCapabilityOwnsTheVariable: a value from config or
// storage env never survives; the hub sets or removes the variable.
func TestApplyConduitCapabilityOwnsTheVariable(t *testing.T) {
	for _, tt := range []struct {
		name string
		on   bool
		in   string
	}{
		{"forged true removed", false, "true"},
		{"forged value replaced", true, "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &HTTPAgentDispatcher{conduitCapability: func() bool { return tt.on }}
			env := map[string]string{envHubConduit: tt.in}
			cls := map[string]api.EnvKind{envHubConduit: api.EnvKindPlain}
			d.applyConduitCapability(env, &cls)
			got, ok := env[envHubConduit]
			_, classified := cls[envHubConduit]
			switch {
			case tt.on && (got != "true" || !classified):
				t.Fatalf("env %q classified %v, want true and classified", got, classified)
			case !tt.on && (ok || classified):
				t.Fatalf("env %q (present %v) classified %v, want removed", got, ok, classified)
			}
		})
	}
}
