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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/localpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMintConduitGrant_SSHDisabled: other stream kinds are unaffected.
func TestMintConduitGrant_SSHDisabled(t *testing.T) {
	f := newConduitFixture(t)
	_, _, err := f.mint(f.owner, grant.StreamHeader{Kind: grant.StreamKindSSH})
	require.NoError(t, err, "ssh grant before the switch")

	f.srv.config.ConduitSSHDisabled = true
	_, _, err = f.mint(f.owner, grant.StreamHeader{Kind: grant.StreamKindSSH})
	assert.ErrorIs(t, err, errConduitForbidden)
	_, _, err = f.mint(f.owner, tcpHeader("3000"))
	assert.NoError(t, err, "tcp")
	_, _, err = f.mint(f.owner, ptyHeader("80", "24"))
	assert.NoError(t, err, "pty")
}

func TestAuthorizeConduitUserStream_SSHDisabled(t *testing.T) {
	f := newConduitFixture(t)
	f.srv.config.ConduitSSHDisabled = true
	verdict, _, err := f.srv.authorizeConduitUserStream(context.Background(), conduitUserStreamAuthz{
		Identity: f.owner, Agent: f.agent, Kind: grant.StreamKindSSH, ViaTunnel: true,
	})
	assert.Equal(t, conduitAuthzDenied, verdict)
	assert.ErrorIs(t, err, errConduitForbidden)
}

// TestAuthorizeConduitTCPTarget_DenyPorts: deny wins over exposed and allowed.
func TestAuthorizeConduitTCPTarget_DenyPorts(t *testing.T) {
	f := newConduitFixture(t) // the agent exposes 3000
	f.srv.config.ConduitTCPAllowedPorts = []int{8080, 8462}
	f.srv.config.ConduitDenyPorts = []int{3000, 8462}
	for port, ok := range map[int]bool{3000: false, 8462: false, 8080: true} {
		err := f.srv.authorizeConduitTCPTarget(f.agent, port)
		if ok {
			assert.NoError(t, err, "port %d", port)
		} else {
			assert.ErrorIs(t, err, errConduitForbidden, "port %d", port)
		}
	}
	_, _, err := f.mint(f.owner, tcpHeader("3000"))
	assert.ErrorIs(t, err, errConduitForbidden, "mint to a denied exposed port")
}

// TestConduitAgentPolicy: the value round-trips through ParsePolicy.
func TestConduitAgentPolicy(t *testing.T) {
	s := &Server{}
	assert.Empty(t, s.conduitAgentPolicy())

	s.config.ConduitSSHDisabled = true
	s.config.ConduitDenyPorts = []int{8462}
	s.config.ConduitDenySockets = []string{"/run/private"}
	p, err := localpolicy.ParsePolicy(s.conduitAgentPolicy())
	require.NoError(t, err)
	assert.True(t, p.SSHDisabled)
	assert.True(t, p.PortDenied(8462))
	assert.False(t, p.PortDenied(8463))
	assert.True(t, p.SocketDenied("/run/private/a.sock"))
}

func TestApplyAgentConduitPolicyOwnsTheVariable(t *testing.T) {
	for _, tt := range []struct{ name, policy string }{
		{name: "forged value removed"},
		{name: "forged value replaced", policy: `{"disable_ssh":true}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{localpolicy.EnvPolicy: "{}"}
			cls := map[string]api.EnvKind{localpolicy.EnvPolicy: api.EnvKindPlain}
			applyAgentConduitPolicy(env, &cls, tt.policy)
			got, ok := env[localpolicy.EnvPolicy]
			_, classified := cls[localpolicy.EnvPolicy]
			if tt.policy == "" {
				assert.False(t, ok || classified, "env %q (present %v) classified %v, want removed", got, ok, classified)
				return
			}
			assert.Equal(t, tt.policy, got)
			assert.True(t, classified)
		})
	}
}

// TestDispatchConduitPolicyEnv: covers create, start and restart.
func TestDispatchConduitPolicyEnv(t *testing.T) {
	const policy = `{"deny_ports":[8462]}`
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
						Env:           map[string]string{localpolicy.EnvPolicy: "{}"},
					},
				}
				if err := p.dispatch(ctx, d, agent); err != nil {
					t.Fatalf("dispatch: %v", err)
				}
				got, ok := p.env(mock)[localpolicy.EnvPolicy]
				if set == "" {
					assert.False(t, ok, "%s = %q, want absent", localpolicy.EnvPolicy, got)
				} else {
					assert.Equal(t, set, got)
				}
			})
		}
	}
}

// TestConduitTunnel_OperatorPolicy: hub- and agent-side refusals both give 403.
func TestConduitTunnel_OperatorPolicy(t *testing.T) {
	t.Run("hub", func(t *testing.T) {
		tf := newTunnelFixture(t)
		tf.srv.config.ConduitSSHDisabled = true
		tf.srv.config.ConduitDenyPorts = []int{3000}
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
	})
	t.Run("agent", func(t *testing.T) {
		tf := newTunnelFixture(t)
		p, err := localpolicy.ParsePolicy(`{"deny_ports":[3000]}`)
		require.NoError(t, err)
		tf.agentPolicy = p
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)
		r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
		require.Equal(t, http.StatusForbidden, r.status, r.body)
		assert.Equal(t, ErrCodeForbidden, r.err.Code, r.body)
		select {
		case conn := <-tf.dials:
			_ = conn.Close()
			t.Fatal("the agent dialed a denied port")
		default:
		}
	})
}
