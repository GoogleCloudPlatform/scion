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

// TestBuildStartContext_ColocatedHubDevToken pins the broker's dev token
// fallback: this process's SCION_AUTH_TOKEN never goes into the agent env,
// and is offered in StartOptions.ColocatedHubDevToken only when the agent has
// no hub-issued token, the dispatch is bound to the co-located hub, and the
// endpoint is operator-derived (TrustedHubEndpoint). The agent manager's
// final endpoint check (agent config and config env overrides) is covered by
// pkg/agent's TestStartColocatedHubDevToken. Each case asserts the hub and
// token keys of the env exactly.
func TestBuildStartContext_ColocatedHubDevToken(t *testing.T) {
	// Made-up values; never real credentials.
	const (
		devToken  = "fake-broker-dev-token"
		hubToken  = "fake.hub.issued"
		bridge    = "http://host.docker.internal:9090"
		reqHub    = "http://localhost:8080"
		bridged   = "http://host.docker.internal:8080"
		remoteHub = "https://hub.example.com"
	)
	local := func() *HubConnection {
		return &HubConnection{Name: "local", IsColocated: true, HubEndpoint: reqHub, Hydrator: newTestHydrator(t)}
	}
	remote := func() *HubConnection {
		return &HubConnection{Name: "remote", HubEndpoint: remoteHub, Hydrator: newTestHydrator(t)}
	}
	tests := []struct {
		name        string
		conns       []func() *HubConnection
		header      string
		in          startContextInputs
		wantOffered bool
		wantTrusted string
		wantEnv     map[string]string
	}{
		{
			name:        "co-located dispatch, bridge-rewritten endpoint: offered",
			conns:       []func() *HubConnection{local},
			header:      "local",
			in:          startContextInputs{HubEndpoint: reqHub},
			wantOffered: true,
			wantTrusted: bridged,
			wantEnv:     map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged},
		},
		{
			name:        "co-located dispatch, creator Config.Env endpoint replaced by the trusted one: offered",
			conns:       []func() *HubConnection{local},
			header:      "local",
			in:          startContextInputs{HubEndpoint: reqHub, Config: &CreateAgentConfig{Env: []string{"SCION_HUB_ENDPOINT=" + remoteHub}}},
			wantOffered: true,
			wantTrusted: bridged,
			wantEnv:     map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged},
		},
		{
			name:        "remote hub dispatch: not offered",
			conns:       []func() *HubConnection{local, remote},
			header:      "remote",
			in:          startContextInputs{HubEndpoint: remoteHub},
			wantTrusted: remoteHub,
			wantEnv:     map[string]string{"SCION_HUB_ENDPOINT": remoteHub, "SCION_HUB_URL": remoteHub},
		},
		{
			name:        "dispatch not bound to the co-located hub (two connections, no header): not offered",
			conns:       []func() *HubConnection{local, remote},
			in:          startContextInputs{HubEndpoint: reqHub},
			wantTrusted: bridged,
			wantEnv:     map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged},
		},
		{
			// One connection and no header: still bound to the co-located hub,
			// but no operator tier supplies the endpoint.
			name:    "co-located dispatch, endpoint only from the resolved env (untrusted): not offered",
			conns:   []func() *HubConnection{local},
			in:      startContextInputs{ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": remoteHub}},
			wantEnv: map[string]string{"SCION_HUB_ENDPOINT": remoteHub, "SCION_HUB_URL": remoteHub},
		},
		{
			name:        "hub-issued AgentToken: unchanged",
			conns:       []func() *HubConnection{local},
			header:      "local",
			in:          startContextInputs{HubEndpoint: reqHub, AgentToken: hubToken},
			wantTrusted: bridged,
			wantEnv:     map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged, "SCION_AUTH_TOKEN": hubToken},
		},
		{
			name:        "hub-issued resolved env token: unchanged",
			conns:       []func() *HubConnection{local},
			header:      "local",
			in:          startContextInputs{HubEndpoint: reqHub, ResolvedEnv: map[string]string{"SCION_AUTH_TOKEN": hubToken}},
			wantTrusted: bridged,
			wantEnv:     map[string]string{"SCION_HUB_ENDPOINT": bridged, "SCION_HUB_URL": bridged, "SCION_AUTH_TOKEN": hubToken},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSCIONEnv(t)
			t.Setenv("SCION_AUTH_TOKEN", devToken)
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			cfg.ContainerHubEndpoint = bridge
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
			if !maps.Equal(got, tt.wantEnv) {
				t.Errorf("hub/token env = %v, want %v", got, tt.wantEnv)
			}
			if sc.Opts.TrustedHubEndpoint != tt.wantTrusted {
				t.Errorf("TrustedHubEndpoint = %q, want %q", sc.Opts.TrustedHubEndpoint, tt.wantTrusted)
			}
			offered := sc.Opts.ColocatedHubDevToken
			switch {
			case tt.wantOffered && offered != devToken:
				t.Errorf("ColocatedHubDevToken: len %d, want the broker dev token", len(offered))
			case !tt.wantOffered && offered != "":
				t.Errorf("ColocatedHubDevToken set (len %d), want empty", len(offered))
			}
			if _, ok := sc.EnvClassifications["SCION_AUTH_TOKEN"]; ok && tt.wantEnv["SCION_AUTH_TOKEN"] == "" {
				t.Error("SCION_AUTH_TOKEN classified although env holds no token")
			}
		})
	}
}
