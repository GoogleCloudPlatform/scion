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

// TestCreateAgentHubEndpointFromProjectSettings tests that hub endpoint is resolved
// from the project's settings.yaml when projectPath is provided.
func TestCreateAgentHubEndpointFromProjectSettings(t *testing.T) {
	t.Run("request hub endpoint takes priority over project settings", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a project directory with settings.yaml containing hub.endpoint
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "project-endpoint-agent",
			"hubEndpoint": "http://localhost:9810",
			"projectPath": "` + projectDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// Request hub endpoint takes priority over project settings (project settings
		// are only a fallback when no endpoint is provided by dispatch/broker).
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from request, got %q", got)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from request, got %q", got)
		}
	})

	t.Run("project settings used when request hub endpoint empty", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  endpoint: "https://hub.example.com"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "project-fallback-agent",
			"projectPath": "` + projectDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com' from project settings, got %q", got)
		}
	})

	t.Run("no project path falls back to request endpoint", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		body := `{
			"name": "no-project-agent",
			"hubEndpoint": "https://hub.direct.com",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.direct.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.direct.com' from request, got %q", got)
		}
	})
}

// TestCreateAgentProjectHubEndpointWhenDisabled tests that hub.enabled=false in
// a project's settings does not suppress the hub endpoint: the broker is
// authoritative about hub connectivity, so the project-settings fallback and
// the dispatcher-provided endpoint both still apply.
func TestCreateAgentProjectHubEndpointWhenDisabled(t *testing.T) {
	t.Run("project hub endpoint used when hub disabled", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a project directory with hub.enabled=false but endpoint configured
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  enabled: false
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "project-disabled-agent",
			"projectPath": "` + projectDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// hub.enabled=false is ignored: the project endpoint is the fallback.
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://scionhub.loophole.site" {
			t.Errorf("expected SCION_HUB_ENDPOINT from project settings despite hub.enabled=false, got %q", got)
		}
	})

	t.Run("dispatcher endpoint still works when project hub disabled", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a project directory with hub.enabled=false
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  enabled: false
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		// Dispatcher provides its own hub endpoint (authoritative in hosted mode)
		body := `{
			"name": "dispatcher-endpoint-agent",
			"hubEndpoint": "https://hub.authoritative.com",
			"projectPath": "` + projectDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// Dispatcher-provided endpoint should still be used (it's authoritative)
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.authoritative.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.authoritative.com' from dispatcher, got %q", got)
		}
	})
}

// TestCreateAgentHubManagedProjectSettingsEndpoint tests that createAgent with a
// hub-managed project (ProjectSlug set, no ProjectPath) correctly resolves the project
// path and uses project settings hub.endpoint from the .scion subdirectory.
func TestCreateAgentHubManagedProjectSettingsEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "http://localhost:9810" // broker's default (combo mode)
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	// Set up a hub-managed project directory at the expected path.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}
	projectPath := filepath.Join(globalDir, "projects", "settings-test-project")
	scionDir := filepath.Join(projectPath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create .scion dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(projectPath) })

	// Place settings.yaml in the .scion subdirectory (hub-managed project layout)
	settingsContent := "hub:\n  endpoint: https://hub.external.example.com\n"
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	// Send createAgent request with projectSlug but no projectPath
	body := `{
		"name": "hub-managed-agent",
		"projectSlug": "settings-test-project",
		"hubEndpoint": "http://localhost:9810",
		"config": {"template": "claude", "workspace": "` + projectPath + `"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set")
	}

	// Request hub endpoint takes priority over project settings (project settings
	// are only a fallback when no endpoint is provided by dispatch/broker).
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from request, got %q", got)
	}
	if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from request, got %q", got)
	}
}

// TestResolveProjectSettingsDir tests the helper function that resolves the
// settings directory for both linked and hub-managed projects.
func TestResolveProjectSettingsDir(t *testing.T) {
	t.Run("linked project - settings at projectPath directly", func(t *testing.T) {
		// Linked project: projectPath = /path/to/project/.scion, settings.yaml is there
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: https://example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		result := resolveProjectSettingsDir(projectDir)
		if result != projectDir {
			t.Errorf("expected %q, got %q", projectDir, result)
		}
	})

	t.Run("hub-managed project - settings in .scion subdirectory", func(t *testing.T) {
		// Hub-managed project: projectPath = ~/.scion/projects/<slug>, settings in .scion/
		projectDir := t.TempDir()
		scionDir := filepath.Join(projectDir, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("failed to create .scion dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("hub:\n  endpoint: https://example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		result := resolveProjectSettingsDir(projectDir)
		if result != scionDir {
			t.Errorf("expected %q (with .scion), got %q", scionDir, result)
		}
	})

	t.Run("no settings file - returns original path", func(t *testing.T) {
		projectDir := t.TempDir()
		result := resolveProjectSettingsDir(projectDir)
		if result != projectDir {
			t.Errorf("expected %q (original path), got %q", projectDir, result)
		}
	})
}

// TestCreateAgentContainerHubEndpointOverride tests that ContainerHubEndpoint
// overrides the dispatcher-provided endpoint for container injection.
func TestCreateAgentContainerHubEndpointOverride(t *testing.T) {
	t.Run("container endpoint overrides request endpoint", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.Debug = true
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
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

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// ContainerHubEndpoint should override the request's localhost value
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:8080" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:8080' from container override, got %q", got)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://host.containers.internal:8080" {
			t.Errorf("expected SCION_HUB_URL='http://host.containers.internal:8080' from container override, got %q", got)
		}
	})

	t.Run("container endpoint overrides localhost even with project settings", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.Debug = true
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		// Create a project directory with settings.yaml containing hub.endpoint
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatal(err)
		}
		settingsContent := `schema_version: "1"
hub:
  enabled: true
  endpoint: "https://tunnel.example.com"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080",
			"projectPath": %q
		}`, projectDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// ContainerHubEndpoint override applies last to localhost endpoints;
		// project settings are only a fallback when no dispatch/broker endpoint exists.
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:8080" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:8080' from container bridge override, got %q", got)
		}
	})

	t.Run("no container endpoint uses request endpoint", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		body := `{
			"name": "test-agent",
			"hubEndpoint": "https://hub.public.com"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Without ContainerHubEndpoint, request endpoint is used
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.public.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.public.com' from request, got %q", got)
		}
	})

	t.Run("non-localhost endpoint is not overridden by container endpoint", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		body := `{
			"name": "test-agent",
			"hubEndpoint": "https://hub.example.com"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Non-localhost endpoint should NOT be overridden by ContainerHubEndpoint
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com' (non-localhost preserved), got %q", got)
		}
	})

	t.Run("kubernetes runtime skips container endpoint override", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "kubernetes"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{
			NameFunc: func() string { return "kubernetes" },
		}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		// Create a project dir with kubernetes settings so resolveManagerForOpts
		// matches the "kubernetes" runtime without trying to create a real manager.
		projectDir := filepath.Join(t.TempDir(), ".scion")
		_ = os.MkdirAll(projectDir, 0755)
		k8sSettings := `schema_version: "1"
profiles:
  local:
    runtime: kubernetes
runtimes:
  kubernetes:
    type: kubernetes
`
		_ = os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(k8sSettings), 0644)

		body := fmt.Sprintf(`{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080",
			"projectPath": %q
		}`, projectDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Kubernetes runtime should NOT use bridge address
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:8080" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:8080' (k8s skips bridge), got %q", got)
		}
	})
}

// TestGCPIdentityBlockAcceptedOnKubernetes_HTTP pins ptone/scion#4034 on all
// three dispatch endpoints: "block" on Kubernetes, from the request or from
// a hub-supplied project or hub default, is accepted and reaches the
// manager with the Kubernetes block identity (here the namespace default
// ServiceAccount, since none is configured) and the block mode, never
// passthrough. The restart path runs the late consistency recheck too.
func TestGCPIdentityBlockAcceptedOnKubernetes_HTTP(t *testing.T) {
	newKubernetesServer := func(t *testing.T) (*Server, *envCapturingManager) {
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
    kubernetes:
        type: kubernetes
`
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ForceRuntime = "kubernetes"
		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
	}

	assertAccepted := func(t *testing.T, w *httptest.ResponseRecorder, mgr *envCapturingManager) {
		t.Helper()
		if w.Code < 200 || w.Code > 299 {
			t.Fatalf("expected a 2xx status, got %d: %s", w.Code, w.Body.String())
		}
		opts := mgr.LastStartOpts()
		if opts.KubernetesBlockIdentity == nil {
			t.Fatalf("expected KubernetesBlockIdentity on the started agent")
		}
		if got := opts.KubernetesBlockIdentity.ServiceAccountName; got != "" {
			t.Errorf("expected the namespace default ServiceAccount (empty), got %q", got)
		}
		if got := mgr.lastEnv["SCION_METADATA_MODE"]; got != "block" {
			t.Errorf("SCION_METADATA_MODE = %q, want block", got)
		}
	}

	t.Run("create", func(t *testing.T) {
		srv, mgr := newKubernetesServer(t)
		body := `{"name": "gcp-block-k8s-agent", "config": {"gcpIdentity": {"metadata_mode": "block"}}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assertAccepted(t, w, mgr)
	})

	t.Run("start", func(t *testing.T) {
		srv, mgr := newKubernetesServer(t)
		body := `{"resolvedEnv": {"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gcp-block-k8s-start/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assertAccepted(t, w, mgr)
	})

	t.Run("restart", func(t *testing.T) {
		srv, mgr := newKubernetesServer(t)
		mgr.agents = []api.AgentInfo{{ID: "gcp-block-k8s-restart", Name: "gcp-block-k8s-restart", Phase: "running"}}
		body := `{"resolvedEnv": {"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gcp-block-k8s-restart/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assertAccepted(t, w, mgr)
	})
}

// TestCreateAgentConnectionHubEndpoint tests that when a request arrives via
// control channel from a specific hub, the connection's hub endpoint is used
// instead of the broker's own config.HubEndpoint (which may point to a
// different hub in multi-hub setups).
func TestCreateAgentConnectionHubEndpoint(t *testing.T) {
	t.Run("connection endpoint used when request endpoint empty", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:8080" // broker's own local hub
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		// Register a remote hub connection (as would happen via control channel)
		srv.hubMu.Lock()
		srv.hubConnections["hub-demo-scion-ai-dev"] = &HubConnection{
			Name:        "hub-demo-scion-ai-dev",
			HubEndpoint: "https://hub.demo.scion-ai.dev",
		}
		srv.hubMu.Unlock()

		// Request comes via control channel with no explicit hubEndpoint
		body := `{
			"name": "remote-hub-agent"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", "hub-demo-scion-ai-dev")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Should use the remote hub's endpoint, NOT the broker's local hub
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.demo.scion-ai.dev" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.demo.scion-ai.dev' from connection, got %q", got)
		}
	})

	t.Run("request endpoint takes priority over connection endpoint", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

		srv.hubMu.Lock()
		srv.hubConnections["hub-demo"] = &HubConnection{
			Name:        "hub-demo",
			HubEndpoint: "https://hub.demo.scion-ai.dev",
		}
		srv.hubMu.Unlock()

		// Request explicitly sets hubEndpoint (hub dispatcher configured it)
		body := `{
			"name": "explicit-endpoint-agent",
			"hubEndpoint": "https://hub.explicit.example.com"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", "hub-demo")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Explicit request endpoint wins over connection
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.explicit.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.explicit.example.com' from request, got %q", got)
		}
	})
}
