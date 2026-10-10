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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// writeGlobalHostCredSetting writes the global settings file under the
// current HOME. content "" removes it.
func writeGlobalHostCredSetting(t *testing.T, content string) {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(globalDir, "settings.yaml")
	if content == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hostCredRequest(header string) *http.Request {
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	if header != "" {
		r.Header.Set("X-Scion-Hub-Connection", header)
	}
	return r
}

func TestColocatedHostCredentials(t *testing.T) {
	local := func() *HubConnection {
		return &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "http://localhost:8080"}
	}
	tests := []struct {
		name     string
		policy   bool
		conn     *HubConnection
		settings string
		want     bool
	}{
		{"co-located loopback policy on", true, local(), "", true},
		{"co-located 127.0.0.1", true, &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "http://127.0.0.1:8080"}, "", true},
		{"co-located ::1", true, &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "http://[::1]:8080"}, "", true},
		{"setting explicitly true", true, local(), "schema_version: \"1\"\nuse_host_credentials: true\n", true},
		{"setting false", true, local(), "schema_version: \"1\"\nuse_host_credentials: false\n", false},
		{"malformed settings fail closed", true, local(), "schema_version: \"1\"\nuse_host_credentials: [x\n", false},
		{"not eligible (hosted or no dev auth)", false, local(), "", false},
		{"not co-located (hosted broker)", true, &HubConnection{Name: "local", HubEndpoint: "http://localhost:8080"}, "", false},
		{"non-loopback endpoint", true, &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "https://hub.example.com"}, "", false},
		{"non-loopback IP", true, &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "http://10.0.0.5:8080"}, "", false},
		{"empty endpoint", true, &HubConnection{Name: "local", IsColocated: true}, "", false},
		{"bare host without scheme", true, &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "localhost:8080"}, "", false},
		{"no connection", true, nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeGlobalHostCredSetting(t, tt.settings)
			s := &Server{config: ServerConfig{HostCredentials: tt.policy}, hubConnections: map[string]*HubConnection{}}
			if tt.conn != nil {
				s.hubConnections[tt.conn.Name] = tt.conn
			}
			if got := s.colocatedHostCredentials(context.Background(), hostCredRequest(""), tt.conn); got != tt.want {
				t.Errorf("colocatedHostCredentials = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestColocatedHostCredentials_ConnectionBinding: a broker holding the
// co-located connection plus a remote-hub connection only injects when the
// request is verifiably bound to the co-located hub. The fallback choice in
// resolveHubConnection and a header that contradicts the HMAC-authenticated
// hub are both refused.
func TestColocatedHostCredentials_ConnectionBinding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	local := &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "http://localhost:8080"}
	remote := &HubConnection{Name: "remote", HubEndpoint: "https://hub.example.com"}
	authCtx := func(name string) context.Context {
		return context.WithValue(context.Background(), authenticatingHubConnCtxKey{}, name)
	}

	two := &Server{config: ServerConfig{HostCredentials: true}, hubConnections: map[string]*HubConnection{"local": local, "remote": remote}}
	one := &Server{config: ServerConfig{HostCredentials: true}, hubConnections: map[string]*HubConnection{"local": local}}

	tests := []struct {
		name   string
		srv    *Server
		ctx    context.Context
		header string
		want   bool
	}{
		{"two connections, no header (fallback choice)", two, context.Background(), "", false},
		{"two connections, header names another connection", two, context.Background(), "remote", false},
		{"two connections, header names local", two, context.Background(), "local", true},
		{"two connections, header local but HMAC by remote", two, authCtx("remote"), "local", false},
		{"two connections, header local and HMAC by local", two, authCtx("local"), "local", true},
		{"two connections, HMAC by local, no header", two, authCtx("local"), "", false},
		{"one connection, no header", one, context.Background(), "", true},
		{"one connection, HMAC by another hub", one, authCtx("remote"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.srv.colocatedHostCredentials(tt.ctx, hostCredRequest(tt.header), local); got != tt.want {
				t.Errorf("colocatedHostCredentials = %v, want %v", got, tt.want)
			}
		})
	}

	// The HMAC identity is also honoured when it is carried only on the
	// request's own context.
	r := hostCredRequest("local").WithContext(authCtx("remote"))
	if two.colocatedHostCredentials(context.Background(), r, local) {
		t.Error("HMAC by remote on the request context must refuse")
	}
}

// TestBuildStartContext_HostCredentialFiles checks that buildStartContext
// sets the non-persisted StartOptions.HostCredentialFiles on every
// operation, including start and restart (no create config), and leaves it
// off for a hosted (non-co-located) connection, a non-loopback hub, or a
// server that is not eligible.
func TestBuildStartContext_HostCredentialFiles(t *testing.T) {
	clearSCIONEnv(t)
	tests := []struct {
		name      string
		policy    bool
		colocated bool
		endpoint  string
		want      bool
	}{
		{"workstation eligible", true, true, "http://localhost:9810", true},
		{"not eligible", false, true, "http://localhost:9810", false},
		{"hosted broker", true, false, "http://localhost:9810", false},
		{"co-located non-loopback hub", true, true, "https://hub.example.com", false},
	}
	for _, tt := range tests {
		for _, o := range hostCredOps {
			t.Run(tt.name+"/"+o.name, func(t *testing.T) {
				srv := newHostCredStartServer(t, tt.policy, tt.colocated, tt.endpoint)
				if got := buildHostCredStartContext(t, srv, o); got != tt.want {
					t.Errorf("HostCredentialFiles = %v, want %v", got, tt.want)
				}
			})
		}
	}
}

// TestBuildStartContext_HostCredentialFiles_SettingFlipBetweenStarts: the
// setting is re-read on every start, so turning it off applies to the next
// start or restart of an agent without restarting the server, and turning it
// back on applies again.
func TestBuildStartContext_HostCredentialFiles_SettingFlipBetweenStarts(t *testing.T) {
	clearSCIONEnv(t)
	for _, o := range hostCredOps {
		t.Run(o.name, func(t *testing.T) {
			srv := newHostCredStartServer(t, true, true, "http://localhost:9810")
			if !buildHostCredStartContext(t, srv, o) {
				t.Fatal("default (unset): want HostCredentialFiles on")
			}
			writeGlobalHostCredSetting(t, "schema_version: \"1\"\nuse_host_credentials: false\n")
			if buildHostCredStartContext(t, srv, o) {
				t.Fatal("after use_host_credentials: false: want HostCredentialFiles off")
			}
			writeGlobalHostCredSetting(t, "schema_version: \"1\"\nuse_host_credentials: true\n")
			if !buildHostCredStartContext(t, srv, o) {
				t.Fatal("after use_host_credentials: true: want HostCredentialFiles on")
			}
		})
	}
}

type hostCredOp struct {
	name string
	op   startOperation
	cfg  *CreateAgentConfig
}

var hostCredOps = []hostCredOp{
	{"create", opCreate, &CreateAgentConfig{}},
	{"start", opHTTPStart, nil},
	{"restart", opHTTPRestart, nil},
}

func newHostCredStartServer(t *testing.T, policy, colocated bool, endpoint string) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HostCredentials = policy
	srv := newTestServerForStartContext(t, cfg)
	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:        "hub-1",
		IsColocated: colocated,
		HubEndpoint: endpoint,
		Hydrator:    newTestHydrator(t),
	}
	srv.hubMu.Unlock()
	return srv
}

func buildHostCredStartContext(t *testing.T, srv *Server, o hostCredOp) bool {
	t.Helper()
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-host-creds",
		Config:      o.cfg,
		HTTPRequest: hostCredRequest("hub-1"),
		Operation:   o.op,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	return sc.Opts.HostCredentialFiles
}
