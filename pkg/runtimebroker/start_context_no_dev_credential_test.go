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
	"maps"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBuildStartContext_AgentTokenIsHubIssuedOnly pins that the start
// context gives an agent the token the hub issued for it, or none: this
// process's SCION_AUTH_TOKEN and SCION_DEV_TOKEN (a dev credential) never
// reach the agent's env or start options, on every endpoint combination
// (co-located dispatch with the bridge rewrite, the public endpoint
// rewrite, the broker's own endpoint, a creator Config.Env endpoint, a
// remote hub, an unbound dispatch, a resolved-env-only endpoint). With a
// hub-issued token the env carries exactly that token. Where the case
// names wantEnv, the hub and token keys of the env are asserted exactly.
func TestBuildStartContext_AgentTokenIsHubIssuedOnly(t *testing.T) {
	// Made-up values; never real credentials.
	const (
		devToken  = "fake-broker-dev-token"
		hubToken  = "fake.hub.issued"
		bridge    = "http://host.docker.internal:9090"
		reqHub    = "http://localhost:8080"
		bridged   = "http://host.docker.internal:8080"
		remoteHub = "https://hub.example.com"
		publicHub = "https://hub-public.example.com"
		brokerHub = "http://localhost:7070"
	)
	local := func() *HubConnection {
		return &HubConnection{Name: "local", IsColocated: true, HubEndpoint: reqHub, Hydrator: newTestHydrator(t)}
	}
	remote := func() *HubConnection {
		return &HubConnection{Name: "remote", HubEndpoint: remoteHub, Hydrator: newTestHydrator(t)}
	}
	tests := []struct {
		name   string
		conns  []func() *HubConnection
		header string
		// cfg adjusts the broker config (ContainerHubEndpoint is preset).
		cfg     func(*ServerConfig)
		in      startContextInputs
		wantTok string // the env's SCION_AUTH_TOKEN; "" means unset
		wantEnv map[string]string
	}{
		{
			name:    "co-located dispatch, bridge-rewritten endpoint, no hub-issued token",
			conns:   []func() *HubConnection{local},
			header:  "local",
			in:      startContextInputs{HubEndpoint: reqHub},
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged},
		},
		{
			name:    "co-located dispatch, creator Config.Env endpoint, no hub-issued token",
			conns:   []func() *HubConnection{local},
			header:  "local",
			in:      startContextInputs{HubEndpoint: reqHub, Config: &CreateAgentConfig{Env: []string{"SCION_HUB_ENDPOINT=" + remoteHub}}},
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged},
		},
		{
			name:   "co-located dispatch, public endpoint rewrite configured, no hub-issued token",
			conns:  []func() *HubConnection{local},
			header: "local",
			cfg: func(c *ServerConfig) {
				c.ColocatedPublicHubEndpoint = publicHub
				c.ColocatedRuntimeHubEndpoints = map[string]string{"docker": bridged}
			},
			in: startContextInputs{HubEndpoint: publicHub},
		},
		{
			name: "no hub connection, endpoint from the broker's own config, no hub-issued token",
			cfg:  func(c *ServerConfig) { c.HubEndpoint = brokerHub },
			in:   startContextInputs{},
		},
		{
			name:    "remote hub dispatch, no hub-issued token",
			conns:   []func() *HubConnection{local, remote},
			header:  "remote",
			in:      startContextInputs{HubEndpoint: remoteHub},
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": remoteHub, "SCION_HUB_URL": remoteHub},
		},
		{
			name:    "dispatch not bound to a hub connection (two connections, no header), no hub-issued token",
			conns:   []func() *HubConnection{local, remote},
			in:      startContextInputs{HubEndpoint: reqHub},
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged},
		},
		{
			name:    "endpoint only from the resolved env, no hub-issued token",
			conns:   []func() *HubConnection{local},
			in:      startContextInputs{ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": remoteHub}},
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": remoteHub, "SCION_HUB_URL": remoteHub},
		},
		{
			name:    "hub-issued AgentToken, co-located: unchanged",
			conns:   []func() *HubConnection{local},
			header:  "local",
			in:      startContextInputs{HubEndpoint: reqHub, AgentToken: hubToken},
			wantTok: hubToken,
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged, "SCION_AUTH_TOKEN": hubToken},
		},
		{
			name:    "hub-issued AgentToken, remote hub: unchanged",
			conns:   []func() *HubConnection{local, remote},
			header:  "remote",
			in:      startContextInputs{HubEndpoint: remoteHub, AgentToken: hubToken},
			wantTok: hubToken,
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": remoteHub, "SCION_HUB_URL": remoteHub, "SCION_AUTH_TOKEN": hubToken},
		},
		{
			name:    "hub-issued resolved env token, co-located: unchanged",
			conns:   []func() *HubConnection{local},
			header:  "local",
			in:      startContextInputs{HubEndpoint: reqHub, ResolvedEnv: map[string]string{"SCION_AUTH_TOKEN": hubToken}},
			wantTok: hubToken,
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged, "SCION_AUTH_TOKEN": hubToken},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSCIONEnv(t)
			t.Setenv("SCION_AUTH_TOKEN", devToken)
			t.Setenv("SCION_DEV_TOKEN", devToken)
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			cfg.ContainerHubEndpoint = bridge
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			srv := newTestServerForStartContextRuntime(t, cfg, "docker")
			srv.hubMu.Lock()
			for _, c := range tt.conns {
				conn := c()
				srv.hubConnections[conn.Name] = conn
			}
			srv.hubMu.Unlock()

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			if tt.header != "" {
				r.Header.Set("X-Scion-Hub-Connection", tt.header)
			}
			in := tt.in
			in.Name = "agent-dev-token"
			in.HTTPRequest = r
			in.Operation = opCreate
			sc, err := srv.buildStartContext(context.Background(), in)
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}

			got := map[string]string{}
			for k, v := range sc.Opts.Env {
				if strings.Contains(v, devToken) {
					t.Errorf("env %s carries the broker dev token", k)
				}
				if strings.HasPrefix(k, "SCION_HUB_") || strings.HasPrefix(k, "SCION_AUTH_") || strings.HasPrefix(k, "SCION_DEV_") {
					got[k] = v
				}
			}
			if tok, ok := got["SCION_AUTH_TOKEN"]; tok != tt.wantTok || (tt.wantTok == "" && ok) {
				t.Errorf("SCION_AUTH_TOKEN: set=%v len %d, want len %d (the hub-issued token or unset)", ok, len(tok), len(tt.wantTok))
			}
			if got["SCION_HUB_ENDPOINT"] == "" {
				t.Error("SCION_HUB_ENDPOINT unset: the case must exercise a hub endpoint")
			}
			if tt.wantEnv != nil && !maps.Equal(got, tt.wantEnv) {
				t.Errorf("hub/token env = %v, want %v", got, tt.wantEnv)
			}
			if _, ok := sc.EnvClassifications["SCION_AUTH_TOKEN"]; ok && tt.wantTok == "" {
				t.Error("SCION_AUTH_TOKEN classified although env holds no token")
			}
		})
	}
}
