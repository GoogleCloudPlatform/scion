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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStartAgentProjectSettingsFallbackHubEndpoint verifies that the startAgent
// handler uses project settings hub.endpoint only as a fallback when no broker
// config or dispatch endpoint is available.
func TestStartAgentProjectSettingsFallbackHubEndpoint(t *testing.T) {
	t.Run("linked project with settings at projectPath", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:9810"
		cfg.Debug = true
		cfg.ForceRuntime = "mock"

		mgr := &provisionCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		// Linked project: projectPath ends in .scion, settings.yaml is directly there
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := "hub:\n  endpoint: https://hub.production.example.com\n"
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		body := fmt.Sprintf(`{"projectPath": %q}`, projectDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}

		if !mgr.startCalled {
			t.Fatal("expected Start to be called")
		}

		// Broker config HubEndpoint takes priority over project settings
		if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
		}
		if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from broker config, got %q", got)
		}
	})

	t.Run("hub-managed project with settings in .scion subdirectory", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:9810"
		cfg.Debug = true
		cfg.ForceRuntime = "mock"

		mgr := &provisionCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		// Hub-managed project: projectPath is the workspace parent (~/.scion/projects/<slug>),
		// settings.yaml lives in the .scion subdirectory
		projectDir := t.TempDir()
		scionDir := filepath.Join(projectDir, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("failed to create .scion dir: %v", err)
		}
		settingsContent := "hub:\n  endpoint: https://hub.native.example.com\n"
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		body := fmt.Sprintf(`{"projectPath": %q}`, projectDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}

		if !mgr.startCalled {
			t.Fatal("expected Start to be called")
		}

		// Broker config HubEndpoint takes priority over project settings
		if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
		}
		if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from broker config, got %q", got)
		}
	})
}

// TestStartAgentBrokerConfigUsedWhenNoProjectSettings verifies that the broker's
// config HubEndpoint is used as fallback when project settings don't specify one.
func TestStartAgentBrokerConfigUsedWhenNoProjectSettings(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	// Create a temp project dir with settings.yaml but no hub endpoint
	projectDir := t.TempDir()
	settingsContent := "harnesses:\n  claude:\n    model: sonnet\n"
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	body := fmt.Sprintf(`{"projectPath": %q}`, projectDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// Without project settings hub.endpoint, broker config should be used
	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
	}
}

// TestStartAgentResolvedEnvHubEndpointFallback verifies that when the broker
// has no HubEndpoint configured, the hub endpoint from resolvedEnv (sent by
// the hub dispatcher) is used as a fallback.
func TestStartAgentResolvedEnvHubEndpointFallback(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "" // Standalone broker without hub endpoint config
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	body := `{"resolvedEnv": {"SCION_HUB_ENDPOINT": "http://hub.example.com:8080", "SCION_PROJECT_ID": "project-1"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// Hub endpoint should fall back to the resolvedEnv value
	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://hub.example.com:8080" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://hub.example.com:8080' from resolvedEnv, got %q", got)
	}
}

// TestStartAgentResolvedEnvHubURLFallback verifies legacy parity: when the broker
// has no HubEndpoint configured, SCION_HUB_URL from resolvedEnv is accepted as
// the fallback endpoint in the start path.
func TestStartAgentResolvedEnvHubURLFallback(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = ""
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	body := `{"resolvedEnv": {"SCION_HUB_URL": "http://hub.example.com:9090"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://hub.example.com:9090" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://hub.example.com:9090' from SCION_HUB_URL fallback, got %q", got)
	}
	if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://hub.example.com:9090" {
		t.Errorf("expected SCION_HUB_URL='http://hub.example.com:9090', got %q", got)
	}
}

// TestStartAgentResolvedEnvHubEndpointWithContainerOverride verifies that when
// the hub endpoint from resolvedEnv is localhost, the ContainerHubEndpoint
// override is applied.
func TestStartAgentResolvedEnvHubEndpointWithContainerOverride(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = ""                                              // No broker-level hub endpoint
	cfg.ContainerHubEndpoint = "http://host.containers.internal:9810" // But has container override
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	// resolvedEnv has localhost endpoint from the hub
	body := `{"resolvedEnv": {"SCION_HUB_ENDPOINT": "http://localhost:9810"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// ContainerHubEndpoint override should be applied since resolvedEnv was localhost
	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:9810', got %q", got)
	}
}

// TestHTTPStartAndRestart_DispatchedEndpointOutranksLocalhostBroker is the
// regression test for GoogleCloudPlatform/scion#1931: on a co-located
// (combo) broker, the broker's own HubEndpoint is often a localhost address
// (its own view of the Hub). On the kubernetes runtime, an agent started or
// restarted through the broker's HTTP handlers must still get the Hub's
// dispatched public endpoint, in parity with what create gives it — not the
// broker's localhost view, which means nothing inside the pod.
func TestHTTPStartAndRestart_DispatchedEndpointOutranksLocalhostBroker(t *testing.T) {
	const dispatched = "https://hub.example.com"

	// A dedicated isolated server, rather than newTestServerWithRuntime: the
	// runtime name here ("kubernetes") must also be the settings-resolved
	// runtime type, or resolveManagerForOpts detects a mismatch against the
	// injected mock runtime and builds a real auxiliary manager instead of
	// using the capturing mock — bypassing the very capture this test needs.
	newSrv := func(t *testing.T) (*Server, *envCapturingManager) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())

		origWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origWd) })

		dotScion := filepath.Join(tmpDir, ".scion")
		if err := os.Mkdir(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		// No explicit "type" for the "kubernetes" runtime entry: ResolveRuntime
		// falls back to the map key as the effective type, so it resolves to
		// "kubernetes" — matching the injected runtime's Name() below — and
		// resolveManagerForOpts uses the broker's own (capturing) manager.
		settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes: {}
`
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
		templatesDir := filepath.Join(dotScion, "templates")
		if err := os.MkdirAll(filepath.Join(templatesDir, "default"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
			t.Fatal(err)
		}

		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:8080" // combo broker's own (loopback) view
		cfg.ForceRuntime = "kubernetes"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
	}

	assertDispatched := func(t *testing.T, mgr *envCapturingManager) {
		t.Helper()
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != dispatched {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want the Hub-dispatched %q (not the broker's own localhost endpoint)", got, dispatched)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != dispatched {
			t.Errorf("SCION_HUB_URL = %q, want the Hub-dispatched %q", got, dispatched)
		}
	}

	t.Run("start", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, dispatched)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})

	t.Run("restart", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, dispatched)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})

	t.Run("start: request field wins over a differing resolvedEnv value", func(t *testing.T) {
		srv, mgr := newSrv(t)
		const stale = "https://stale-in-resolved-env.example.com"
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, stale)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})

	t.Run("restart: request field wins over a differing resolvedEnv value", func(t *testing.T) {
		srv, mgr := newSrv(t)
		const stale = "https://stale-in-resolved-env.example.com"
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, stale)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})
}

// TestHTTPStartAndRestart_DockerComboBridgeOverrideMatchesCreate proves
// parity for the docker combo case in GoogleCloudPlatform/scion#1931: create
// sends the dispatched endpoint as its request-level HubEndpoint; start and
// restart send it as their own request-level HubEndpoint and in
// ResolvedEnv["SCION_HUB_ENDPOINT"], as the Hub does; and — for both a
// public dispatched endpoint and a localhost one that then goes through the
// container bridge override — all three operations resolve to the same
// SCION_HUB_ENDPOINT for the broker's ContainerHubEndpoint configuration.
func TestHTTPStartAndRestart_DockerComboBridgeOverrideMatchesCreate(t *testing.T) {
	newSrv := func() (*Server, *envCapturingManager) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:8080" // combo broker's own (loopback) view
		cfg.ContainerHubEndpoint = "http://host.docker.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
	}

	cases := []struct {
		name       string
		dispatched string
		want       string
	}{
		{name: "public dispatched endpoint", dispatched: "https://hub.example.com", want: "https://hub.example.com"},
		{name: "localhost dispatched endpoint goes through the bridge override", dispatched: "http://localhost:8080", want: "http://host.docker.internal:8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				srv, mgr := newSrv()
				body := fmt.Sprintf(`{"name": "test-agent", "hubEndpoint": %q}`, tc.dispatched)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusCreated {
					t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
				}
				if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != tc.want {
					t.Errorf("create: SCION_HUB_ENDPOINT = %q, want %q", got, tc.want)
				}
			})

			t.Run("http-start", func(t *testing.T) {
				srv, mgr := newSrv()
				body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, tc.dispatched, tc.dispatched)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusAccepted {
					t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
				}
				if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != tc.want {
					t.Errorf("http-start: SCION_HUB_ENDPOINT = %q, want %q (same as create)", got, tc.want)
				}
			})

			t.Run("http-restart", func(t *testing.T) {
				srv, mgr := newSrv()
				body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, tc.dispatched, tc.dispatched)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusAccepted {
					t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
				}
				if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != tc.want {
					t.Errorf("http-restart: SCION_HUB_ENDPOINT = %q, want %q (same as create)", got, tc.want)
				}
			})
		})
	}
}

// TestHTTPStartAndRestart_ConnectionHeaderRescuesLocalhostDispatch proves the
// connection-endpoint localhost rescue is wired all the way from the
// X-Scion-Hub-Connection header through the HTTP start and restart handlers,
// not just inside the resolver: a registered connection naming a public
// endpoint rescues a localhost dispatched value on kubernetes, the same way
// it does on create with the same header.
func TestHTTPStartAndRestart_ConnectionHeaderRescuesLocalhostDispatch(t *testing.T) {
	const (
		connectionName = "conn1"
		connEndpoint   = "https://hub.connection.example.com"
		dispatchLocal  = "http://localhost:9090"
	)

	// A dedicated isolated server: the runtime name here ("kubernetes") must
	// also be the settings-resolved runtime type, or resolveManagerForOpts
	// detects a mismatch against the injected mock runtime and builds a real
	// auxiliary manager instead of using the capturing mock.
	newSrv := func(t *testing.T) (*Server, *envCapturingManager) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())

		origWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origWd) })

		dotScion := filepath.Join(tmpDir, ".scion")
		if err := os.Mkdir(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes: {}
`
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
		templatesDir := filepath.Join(dotScion, "templates")
		if err := os.MkdirAll(filepath.Join(templatesDir, "default"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
			t.Fatal(err)
		}

		creds := makeTestCreds(connectionName, "test-broker-id", connEndpoint)
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEnabled = true
		cfg.HubEndpoint = "http://localhost:8080" // combo broker's own (loopback) view
		cfg.InMemoryCredentials = creds
		cfg.BrokerAuthEnabled = false
		cfg.ForceRuntime = "kubernetes"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
	}

	assertRescued := func(t *testing.T, mgr *envCapturingManager) {
		t.Helper()
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != connEndpoint {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want the connection endpoint %q (not the broker's own localhost, and not the raw localhost dispatch)", got, connEndpoint)
		}
	}

	t.Run("create", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"name": "test-agent", "hubEndpoint": %q}`, dispatchLocal)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}
		assertRescued(t, mgr)
	})

	t.Run("http-start", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatchLocal, dispatchLocal)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertRescued(t, mgr)
	})

	t.Run("http-restart", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatchLocal, dispatchLocal)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertRescued(t, mgr)
	})
}

// TestRestartAgent_ContainerScanSuppliesSettingsFallback proves restartAgent's
// container-scan branch (used when the request carries no projectPath) feeds
// the scanned agent's ProjectPath into buildStartContext, so the
// project-settings fallback reaches the resolver on restart and is not
// silently dropped by an empty ProjectPath.
func TestRestartAgent_ContainerScanSuppliesSettingsFallback(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: https://settings.example.com\n"), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	// No broker-level HubEndpoint: the settings fallback must supply it.

	mgr := &envCapturingManager{}
	mgr.agents = []api.AgentInfo{
		{Name: "test-agent", ProjectPath: projectDir, Labels: map[string]string{"scion.project_id": "p1"}},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart?projectId=p1", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://settings.example.com" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want the settings-supplied endpoint %q (container-scan projectPath must reach the resolver)", got, "https://settings.example.com")
	}
}

// TestCreateAgentPortPreservedAcrossBridge verifies that when the hub dispatch
// sends a localhost endpoint on port 8080 but the broker's ContainerHubEndpoint
// was pre-computed with port 9810, the actual endpoint port (8080) is preserved.
func TestCreateAgentPortPreservedAcrossBridge(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	// Simulate the bug scenario: ContainerHubEndpoint was auto-computed
	// from a standalone hub port (9810), but the hub actually serves on
	// the web port (8080) in combo mode.
	cfg.ContainerHubEndpoint = "http://host.containers.internal:9810"
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	body := `{
		"name": "test-agent",
		"hubEndpoint": "http://localhost:8080"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// The bridge host should be applied but the port from the actual
	// endpoint (8080) must be preserved, not the pre-computed port (9810).
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:8080" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:8080' (port preserved), got %q", got)
	}
}

func TestIsLocalhostEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"http://localhost:8080", true},
		{"https://localhost:443", true},
		{"http://localhost", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.0.0.1", true},
		{"http://[::1]:8080", true},
		{"http://[::1]", true},
		{"https://hub.example.com", false},
		{"https://hub.example.com:8080", false},
		{"http://host.containers.internal:8080", false},
		{"http://192.168.1.100:8080", false},
		{"", false},
		{"not-a-url", false},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			if got := isLocalhostEndpoint(tt.endpoint); got != tt.want {
				t.Errorf("isLocalhostEndpoint(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}
