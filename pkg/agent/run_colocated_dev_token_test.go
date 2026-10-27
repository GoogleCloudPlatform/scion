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

package agent

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Made-up values; never real credentials.
const (
	fakeColocatedDevToken = "fake-colocated-dev-token"
	fakeHostDevToken      = "fake-host-dev-token"
	fakeHubIssuedToken    = "fake.hub.issued"
)

// TestStartColocatedHubDevToken pins when the co-located hub's dev token
// becomes the agent's hub token: only for a broker-mode start whose final hub
// endpoint is still the broker's trusted endpoint. Every case also plants a
// dev token in the host env and in ~/.scion/dev-token, which must never reach
// the agent. Each case asserts the hub and token keys of the container env
// exactly, and the agent token file.
func TestStartColocatedHubDevToken(t *testing.T) {
	const (
		bridgeEndpoint = "http://host.docker.internal:8080"
		remoteEndpoint = "https://hub.example.com"
		agentEndpoint  = "http://agent-config.example:9810"
		configEndpoint = "http://config-env.example:9810"
	)
	tests := []struct {
		name string
		// agentHub and configEnvHub are the agent config's hub.endpoint and
		// env.SCION_HUB_ENDPOINT ("" = unset).
		agentHub, configEnvHub string
		opts                   api.StartOptions
		// wantToken is the token file content; "" means no token file.
		wantToken    string
		wantEndpoint string
		// wantURL is the container's SCION_HUB_URL; "" means wantEndpoint.
		wantURL string
	}{
		{
			name: "co-located dispatch, bridge-rewritten endpoint: injected",
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantToken:    fakeColocatedDevToken,
			wantEndpoint: bridgeEndpoint,
		},
		{
			name: "remote hub (no co-located token offered): not injected",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint},
				TrustedHubEndpoint: remoteEndpoint,
			},
			wantEndpoint: remoteEndpoint,
		},
		{
			name:     "agent config hub.endpoint override: not injected",
			agentHub: agentEndpoint,
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantEndpoint: agentEndpoint,
		},
		{
			name:         "config env SCION_HUB_ENDPOINT override: not injected",
			configEnvHub: configEndpoint,
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantEndpoint: configEndpoint,
		},
		{
			name:     "agent config hub.endpoint equal to the trusted endpoint: injected",
			agentHub: bridgeEndpoint,
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantToken:    fakeColocatedDevToken,
			wantEndpoint: bridgeEndpoint,
		},
		{
			name:         "config env SCION_HUB_ENDPOINT equal to the trusted endpoint: injected",
			configEnvHub: bridgeEndpoint,
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantToken:    fakeColocatedDevToken,
			wantEndpoint: bridgeEndpoint,
		},
		{
			name: "hub-issued token present: unchanged",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint,
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: bridgeEndpoint,
		},
		{
			name: "hub-issued token, remote hub: unchanged",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint,
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint: remoteEndpoint,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: remoteEndpoint,
		},
		{
			name: "no trusted endpoint: not injected",
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantEndpoint: bridgeEndpoint,
		},
		{
			name: "SCION_HUB_URL differs from the trusted endpoint: not injected",
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": remoteEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantEndpoint: bridgeEndpoint,
			wantURL:      remoteEndpoint,
		},
		{
			name: "SCION_HUB_ENDPOINT differs from the trusted endpoint: not injected",
			opts: api.StartOptions{
				BrokerMode:           true,
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint:   bridgeEndpoint,
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantEndpoint: remoteEndpoint,
			wantURL:      bridgeEndpoint,
		},
		{
			name: "not broker mode: not injected",
			opts: api.StartOptions{
				Env:                  map[string]string{"SCION_HUB_ENDPOINT": "http://localhost:9810", "SCION_HUB_URL": "http://localhost:9810"},
				TrustedHubEndpoint:   "http://localhost:9810",
				ColocatedHubDevToken: fakeColocatedDevToken,
			},
			wantEndpoint: "http://localhost:9810",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const agentName = "dev-token-agent"
			projectScionDir := setupTrustedHubEndpointTestProject(t, agentName, tt.agentHub, tt.configEnvHub)
			t.Setenv("SCION_DEV_TOKEN", fakeHostDevToken)
			home := os.Getenv("HOME")
			if err := os.WriteFile(filepath.Join(home, ".scion", "dev-token"), []byte(fakeHostDevToken), 0o600); err != nil {
				t.Fatal(err)
			}

			var captured runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					captured = cfg
					return "mock-id", nil
				},
			})
			opts := tt.opts
			opts.Name = agentName
			opts.ProjectPath = projectScionDir
			opts.NoAuth = true
			if _, err := mgr.Start(context.Background(), opts); err != nil {
				t.Fatalf("Start: %v", err)
			}

			got := map[string]string{}
			for _, e := range captured.Env {
				k, v, _ := strings.Cut(e, "=")
				for _, tok := range []string{fakeColocatedDevToken, fakeHostDevToken, fakeHubIssuedToken} {
					if strings.Contains(v, tok) {
						t.Errorf("container env %s carries a token value", k)
					}
				}
				if strings.HasPrefix(k, "SCION_HUB_") || strings.HasPrefix(k, "SCION_AUTH_") || strings.HasPrefix(k, "SCION_DEV_") {
					got[k] = v
				}
			}
			wantURL := tt.wantURL
			if wantURL == "" {
				wantURL = tt.wantEndpoint
			}
			want := map[string]string{"SCION_HUB_ENDPOINT": tt.wantEndpoint, "SCION_HUB_URL": wantURL}
			if !maps.Equal(got, want) {
				t.Errorf("container hub/token env = %v, want %v", got, want)
			}
			if v, ok := opts.Env["SCION_AUTH_TOKEN"]; ok {
				t.Errorf("SCION_AUTH_TOKEN left in opts.Env (len %d)", len(v))
			}

			tokenPath := filepath.Join(captured.HomeDir, ".scion", "scion-token")
			data, err := os.ReadFile(tokenPath)
			switch {
			case tt.wantToken == "":
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("agent token file: err = %v, want not exist", err)
				}
			case err != nil:
				t.Fatalf("read agent token file: %v", err)
			case string(data) != tt.wantToken:
				t.Errorf("agent token file holds the wrong token (len %d, want len %d)", len(data), len(tt.wantToken))
			}
		})
	}
}
