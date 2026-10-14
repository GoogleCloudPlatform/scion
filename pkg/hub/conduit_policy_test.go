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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthorizeConduitTCPTarget_DenyPorts: deny wins over exposed and allowed.
func TestAuthorizeConduitTCPTarget_DenyPorts(t *testing.T) {
	f := newConduitFixture(t) // the agent exposes 3000
	f.srv.config.ConduitTCPAllowedPorts = []int{8080, 9999}
	f.srv.config.ConduitPolicy.DenyPorts = []int{3000, 9999}
	for port, ok := range map[int]bool{3000: false, 9999: false, 8080: true} {
		err := f.srv.authorizeConduitTCPTarget(f.agent, port)
		if ok {
			assert.NoError(t, err, "port %d", port)
		} else {
			assert.ErrorIs(t, err, errConduitForbidden, "port %d", port)
		}
	}
}

// TestConduitAgentPolicy: the value round-trips through ParsePolicy.
func TestConduitAgentPolicy(t *testing.T) {
	s := &Server{}
	assert.Empty(t, s.conduitAgentPolicy())

	s.config.ConduitPolicy = conduit.Policy{DisableSSH: true, DenyPorts: []int{9999}, DisableSSHSocketForwarding: true}
	p, err := conduit.ParsePolicy(s.conduitAgentPolicy())
	require.NoError(t, err)
	assert.Equal(t, s.config.ConduitPolicy, p)
}

// TestDispatchConduitPolicyEnv: covers create, start and restart.
func TestDispatchConduitPolicyEnv(t *testing.T) {
	const policy = `{"deny_ports":[9999]}`
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
	for _, p := range paths {
		for _, set := range []string{"", policy} {
			t.Run(p.name+"/"+set, func(t *testing.T) {
				ctx := context.Background()
				memStore := createTestStore(t)
				id := "conduit-policy-" + p.name
				if err := memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
					ID: tid("broker-" + id), Name: "b", Slug: "b",
					Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
				}); err != nil {
					t.Fatal(err)
				}
				mock := &mockRuntimeBrokerClient{}
				d := NewHTTPAgentDispatcherWithClient(memStore, mock, false, slog.Default())
				d.SetConduitPolicyProvider(func() string { return set })
				agent := &store.Agent{
					ID: tid("agent-" + id), Name: id, Slug: id,
					ProjectID: tid("project-" + id), OwnerID: tid("user-" + id),
					RuntimeBrokerID: tid("broker-" + id),
					AppliedConfig: &store.AgentAppliedConfig{
						HarnessConfig: "claude",
						Env:           map[string]string{conduit.EnvPolicy: "{}"},
					},
				}
				if err := p.dispatch(ctx, d, agent); err != nil {
					t.Fatalf("dispatch: %v", err)
				}
				got, ok := p.env(mock)[conduit.EnvPolicy]
				if set == "" {
					assert.False(t, ok, "%s = %q, want absent", conduit.EnvPolicy, got)
				} else {
					assert.Equal(t, set, got)
				}
			})
		}
	}
}

func TestConduitTunnel_OperatorPolicy(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.srv.config.ConduitPolicy = conduit.Policy{DisableSSH: true, DenyPorts: []int{3000}}
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)
	for _, k := range []struct {
		kind   string
		params map[string]string
	}{{grant.StreamKindSSH, nil}, {grant.StreamKindTCP, tcpParams("3000")}} {
		r := c.post(t, tf.launched.ID, k.kind, k.params)
		require.Equal(t, http.StatusForbidden, r.status, r.body)
		assert.Equal(t, ErrCodeForbidden, r.err.Code, r.body)
	}
	select {
	case conn := <-tf.dials:
		_ = conn.Close()
		t.Fatal("the agent dialed a denied port")
	default:
	}
}
