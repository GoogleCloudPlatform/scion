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
	"net/http/httptest"
	"testing"
)

func TestColocatedHostCredentials(t *testing.T) {
	tests := []struct {
		name   string
		policy bool
		conn   *HubConnection
		want   bool
	}{
		{"co-located loopback policy on", true, &HubConnection{IsColocated: true, HubEndpoint: "http://localhost:8080"}, true},
		{"co-located 127.0.0.1", true, &HubConnection{IsColocated: true, HubEndpoint: "http://127.0.0.1:8080"}, true},
		{"co-located ::1", true, &HubConnection{IsColocated: true, HubEndpoint: "http://[::1]:8080"}, true},
		{"policy off", false, &HubConnection{IsColocated: true, HubEndpoint: "http://localhost:8080"}, false},
		{"not co-located (hosted broker)", true, &HubConnection{IsColocated: false, HubEndpoint: "http://localhost:8080"}, false},
		{"non-loopback endpoint", true, &HubConnection{IsColocated: true, HubEndpoint: "https://hub.example.com"}, false},
		{"non-loopback IP", true, &HubConnection{IsColocated: true, HubEndpoint: "http://10.0.0.5:8080"}, false},
		{"empty endpoint", true, &HubConnection{IsColocated: true}, false},
		{"bare host without scheme", true, &HubConnection{IsColocated: true, HubEndpoint: "localhost:8080"}, false},
		{"no connection", true, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{config: ServerConfig{HostCredentials: tt.policy}}
			if got := s.colocatedHostCredentials(tt.conn); got != tt.want {
				t.Errorf("colocatedHostCredentials = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuildStartContext_HostCredentialFiles checks that buildStartContext
// sets the non-persisted StartOptions.HostCredentialFiles from the policy on
// every operation, including start and restart (no create config), and
// leaves it off for a hosted (non-co-located) connection or with the policy
// off.
func TestBuildStartContext_HostCredentialFiles(t *testing.T) {
	clearSCIONEnv(t)
	tests := []struct {
		name      string
		policy    bool
		colocated bool
		endpoint  string
		want      bool
	}{
		{"workstation policy on", true, true, "http://localhost:9810", true},
		{"workstation policy off", false, true, "http://localhost:9810", false},
		{"hosted broker", true, false, "http://localhost:9810", false},
		{"co-located non-loopback hub", true, true, "https://hub.example.com", false},
	}
	ops := []struct {
		name string
		op   startOperation
		cfg  *CreateAgentConfig
	}{
		{"create", opCreate, &CreateAgentConfig{}},
		{"start", opHTTPStart, nil},
		{"restart", opHTTPRestart, nil},
	}
	for _, tt := range tests {
		for _, o := range ops {
			t.Run(tt.name+"/"+o.name, func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				cfg.HostCredentials = tt.policy
				srv := newTestServerForStartContext(t, cfg)
				srv.hubMu.Lock()
				srv.hubConnections["hub-1"] = &HubConnection{
					Name:        "hub-1",
					IsColocated: tt.colocated,
					HubEndpoint: tt.endpoint,
					Hydrator:    newTestHydrator(t),
				}
				srv.hubMu.Unlock()

				r := httptest.NewRequest("POST", "/api/v1/agents", nil)
				r.Header.Set("X-Scion-Hub-Connection", "hub-1")
				sc, err := srv.buildStartContext(context.Background(), startContextInputs{
					Name:        "agent-host-creds",
					Config:      o.cfg,
					HTTPRequest: r,
					Operation:   o.op,
				})
				if err != nil {
					t.Fatalf("buildStartContext: %v", err)
				}
				if sc.Opts.HostCredentialFiles != tt.want {
					t.Errorf("HostCredentialFiles = %v, want %v", sc.Opts.HostCredentialFiles, tt.want)
				}
			})
		}
	}
}
