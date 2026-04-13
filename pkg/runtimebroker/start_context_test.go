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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func newTestServerForStartContext(t *testing.T, cfg ServerConfig) *Server {
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
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create dummy templates to satisfy FindTemplate
	templatesDir := filepath.Join(dotScion, "templates")
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg.ForceRuntime = "mock"
	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{}
	return New(cfg, mgr, rt)
}

func TestBuildStartContext_BasicFields(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-broker"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()

	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "my-agent",
		AgentID:     "uuid-1",
		Slug:        "my-agent-slug",
		GroveID:     "grove-1",
		Attach:      false,
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Name != "my-agent" {
		t.Errorf("expected name 'my-agent', got %q", sc.Opts.Name)
	}
	if !sc.Opts.BrokerMode {
		t.Error("expected BrokerMode to be true")
	}
	if sc.Opts.Detached == nil || !*sc.Opts.Detached {
		t.Error("expected Detached to be true when Attach=false")
	}

	// Verify broker identity env
	if sc.Opts.Env["SCION_BROKER_NAME"] != "test-broker" {
		t.Errorf("expected SCION_BROKER_NAME='test-broker', got %q", sc.Opts.Env["SCION_BROKER_NAME"])
	}
	if sc.Opts.Env["SCION_BROKER_ID"] != "broker-1" {
		t.Errorf("expected SCION_BROKER_ID='broker-1', got %q", sc.Opts.Env["SCION_BROKER_ID"])
	}
	if sc.Opts.Env["SCION_AGENT_ID"] != "uuid-1" {
		t.Errorf("expected SCION_AGENT_ID='uuid-1', got %q", sc.Opts.Env["SCION_AGENT_ID"])
	}
	if sc.Opts.Env["SCION_AGENT_SLUG"] != "my-agent-slug" {
		t.Errorf("expected SCION_AGENT_SLUG='my-agent-slug', got %q", sc.Opts.Env["SCION_AGENT_SLUG"])
	}
	if sc.Opts.Env["SCION_GROVE_ID"] != "grove-1" {
		t.Errorf("expected SCION_GROVE_ID='grove-1', got %q", sc.Opts.Env["SCION_GROVE_ID"])
	}
	if sc.Opts.Env["SCION_DEBUG"] != "1" {
		t.Errorf("expected SCION_DEBUG='1', got %q", sc.Opts.Env["SCION_DEBUG"])
	}
}

func TestBuildStartContext_EnvMerging(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		ResolvedEnv: map[string]string{
			"KEY_A": "from-hub",
			"KEY_B": "from-hub",
		},
		Config: &CreateAgentConfig{
			Env: []string{"KEY_B=from-config", "KEY_C=from-config"},
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	// ResolvedEnv is applied first, Config.Env overrides
	if sc.Opts.Env["KEY_A"] != "from-hub" {
		t.Errorf("expected KEY_A='from-hub', got %q", sc.Opts.Env["KEY_A"])
	}
	if sc.Opts.Env["KEY_B"] != "from-config" {
		t.Errorf("expected KEY_B='from-config' (config overrides hub), got %q", sc.Opts.Env["KEY_B"])
	}
	if sc.Opts.Env["KEY_C"] != "from-config" {
		t.Errorf("expected KEY_C='from-config', got %q", sc.Opts.Env["KEY_C"])
	}
}

func TestBuildStartContext_TelemetryOverride(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		ResolvedEnv: map[string]string{
			"SCION_TELEMETRY_ENABLED": "true",
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.TelemetryOverride == nil || !*sc.Opts.TelemetryOverride {
		t.Error("expected TelemetryOverride to be true when SCION_TELEMETRY_ENABLED=true")
	}
}

func TestBuildStartContext_ResolvedSecrets(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Value: "secret-value"},
	}
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:            "agent-1",
		ResolvedSecrets: secrets,
		HTTPRequest:     r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Opts.ResolvedSecrets) != 1 || sc.Opts.ResolvedSecrets[0].Name != "API_KEY" {
		t.Errorf("expected resolved secrets to be passed through, got %v", sc.Opts.ResolvedSecrets)
	}
}

func TestBuildStartContext_ConfigFields(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		Config: &CreateAgentConfig{
			Template:      "my-template",
			Image:         "my-image:latest",
			HarnessConfig: "claude",
			HarnessAuth:   "api-key",
			Task:          "write tests",
			Workspace:     "/workspace",
			Profile:       "default",
			Branch:        "feature-1",
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Template != "my-template" {
		t.Errorf("expected Template='my-template', got %q", sc.Opts.Template)
	}
	if sc.Opts.Image != "my-image:latest" {
		t.Errorf("expected Image='my-image:latest', got %q", sc.Opts.Image)
	}
	if sc.Opts.HarnessConfig != "claude" {
		t.Errorf("expected HarnessConfig='claude', got %q", sc.Opts.HarnessConfig)
	}
	if sc.Opts.Task != "write tests" {
		t.Errorf("expected Task='write tests', got %q", sc.Opts.Task)
	}
	if sc.TemplateSlug != "my-template" {
		t.Errorf("expected TemplateSlug='my-template', got %q", sc.TemplateSlug)
	}
}

func TestBuildStartContext_GitClone(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		GrovePath: "/some/path",
		Config: &CreateAgentConfig{
			Branch: "feature-1",
			GitClone: &api.GitCloneConfig{
				URL:    "https://github.com/org/repo.git",
				Branch: "main",
				Depth:  1,
			},
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_GIT_CLONE_URL"] != "https://github.com/org/repo.git" {
		t.Errorf("expected SCION_GIT_CLONE_URL set, got %q", sc.Opts.Env["SCION_GIT_CLONE_URL"])
	}
	if sc.Opts.Env["SCION_GIT_BRANCH"] != "main" {
		t.Errorf("expected SCION_GIT_BRANCH='main', got %q", sc.Opts.Env["SCION_GIT_BRANCH"])
	}
	if sc.Opts.Env["SCION_GIT_DEPTH"] != "1" {
		t.Errorf("expected SCION_GIT_DEPTH='1', got %q", sc.Opts.Env["SCION_GIT_DEPTH"])
	}
	if sc.Opts.Env["SCION_AGENT_BRANCH"] != "feature-1" {
		t.Errorf("expected SCION_AGENT_BRANCH='feature-1', got %q", sc.Opts.Env["SCION_AGENT_BRANCH"])
	}
	// Git clone mode should clear workspace but preserve grove path
	// so ProvisionAgent can resolve the correct agent directory.
	if sc.Opts.Workspace != "" {
		t.Errorf("expected Workspace to be empty in git clone mode, got %q", sc.Opts.Workspace)
	}
	if sc.Opts.GrovePath != "/some/path" {
		t.Errorf("expected GrovePath to be preserved in git clone mode, got %q", sc.Opts.GrovePath)
	}
}

func TestBuildStartContext_NilHTTPRequest(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Should not panic with nil HTTPRequest
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Name != "agent-1" {
		t.Errorf("expected name 'agent-1', got %q", sc.Opts.Name)
	}
}

func TestBuildStartContext_AttachMode(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:   "agent-1",
		Attach: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Detached == nil || *sc.Opts.Detached {
		t.Error("expected Detached to be false when Attach=true")
	}
}

func TestBuildStartContext_HubNativeGroveWritesMarker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate a hub-native grove: GroveSlug set, GrovePath pre-resolved
	// (as the createAgent handler does for env-gather), and GroveID from hub.
	grovesDir := t.TempDir()
	grovePath := filepath.Join(grovesDir, "web-demo")

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		GroveSlug: "web-demo",
		GrovePath: grovePath,
		GroveID:   "6d868c0f-b862-49e0-a44b-3555a3887ee3",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify .scion marker file was created (not a directory)
	scionPath := filepath.Join(grovePath, ".scion")
	if !config.IsGroveMarkerFile(scionPath) {
		t.Fatal(".scion marker file was not created")
	}

	// Verify grove-id was written via marker
	marker, err := config.ReadGroveMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read .scion marker: %v", err)
	}
	if marker.GroveID != "6d868c0f-b862-49e0-a44b-3555a3887ee3" {
		t.Errorf("expected grove-id '6d868c0f-b862-49e0-a44b-3555a3887ee3', got %q", marker.GroveID)
	}
	if marker.GroveSlug != "web-demo" {
		t.Errorf("expected grove-slug 'web-demo', got %q", marker.GroveSlug)
	}

	// Verify external grove-configs directories were created
	extPath, err := marker.ExternalGrovePath()
	if err != nil {
		t.Fatalf("failed to get external grove path: %v", err)
	}
	if extPath == "" {
		t.Fatal("expected non-empty external grove path")
	}
	extAgents := filepath.Join(extPath, "agents")
	if _, err := os.Stat(extAgents); os.IsNotExist(err) {
		t.Fatalf("external agents dir was not created: %s", extAgents)
	}

	// GrovePath should be passed through to opts
	if sc.Opts.GrovePath != grovePath {
		t.Errorf("expected GrovePath %q, got %q", grovePath, sc.Opts.GrovePath)
	}
}

func TestBuildStartContext_HubNativeGroveSlugResolution(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate: GroveSlug set, GrovePath empty (buildStartContext resolves it),
	// GroveID from hub. This is the path when the handler doesn't pre-resolve.
	t.Setenv("HOME", t.TempDir())

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		GroveSlug: "my-project",
		GroveID:   "aabbccdd-1234-5678-9012-abcdef123456",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify grove-id was written via marker file
	scionPath := filepath.Join(sc.Opts.GrovePath, ".scion")
	marker, err := config.ReadGroveMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read .scion marker: %v", err)
	}
	if marker.GroveID != "aabbccdd-1234-5678-9012-abcdef123456" {
		t.Errorf("expected grove-id from hub, got %q", marker.GroveID)
	}
}

func TestBuildStartContext_HubNativeGrovePreservesExistingGroveID(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Pre-create .scion as a directory with an existing grove-id (git grove)
	grovePath := filepath.Join(t.TempDir(), "existing-grove")
	scionDir := filepath.Join(grovePath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	existingID := "existing-id-1234-5678"
	if err := config.WriteGroveID(scionDir, existingID); err != nil {
		t.Fatal(err)
	}

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		GroveSlug: "existing-grove",
		GrovePath: grovePath,
		GroveID:   "new-id-from-hub",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify existing grove-id was NOT overwritten (directory-based path)
	groveID, err := config.ReadGroveID(scionDir)
	if err != nil {
		t.Fatalf("failed to read grove-id: %v", err)
	}
	if groveID != existingID {
		t.Errorf("expected existing grove-id %q to be preserved, got %q", existingID, groveID)
	}
}

func TestBuildStartContext_HubNativeGrovePreservesExistingMarker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Pre-create .scion as a marker file (hub-native grove)
	grovePath := filepath.Join(t.TempDir(), "existing-grove")
	if err := os.MkdirAll(grovePath, 0755); err != nil {
		t.Fatal(err)
	}
	existingID := "existing-id-1234-5678"
	scionPath := filepath.Join(grovePath, ".scion")
	if err := config.WriteGroveMarker(scionPath, &config.GroveMarker{
		GroveID:   existingID,
		GroveName: "existing-grove",
		GroveSlug: "existing-grove",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		GroveSlug: "existing-grove",
		GrovePath: grovePath,
		GroveID:   "new-id-from-hub",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify existing marker was NOT overwritten (marker file path)
	marker, err := config.ReadGroveMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read marker: %v", err)
	}
	if marker.GroveID != existingID {
		t.Errorf("expected existing grove-id %q to be preserved, got %q", existingID, marker.GroveID)
	}
}

func TestBuildStartContext_HubEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.HubEndpoint = "https://hub.example.com"
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Without HTTPRequest, uses resolveHubEndpointForStart path
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Env["SCION_HUB_ENDPOINT"] != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", sc.Opts.Env["SCION_HUB_ENDPOINT"])
	}
	if sc.Opts.Env["SCION_HUB_URL"] != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_URL='https://hub.example.com', got %q", sc.Opts.Env["SCION_HUB_URL"])
	}
}

func TestBuildStartContext_GCPMetadataDefaultBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// No GCPIdentity config — should default to block mode
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-no-gcp",
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block' by default, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_PORT"] != "18380" {
		t.Errorf("expected SCION_METADATA_PORT='18380', got %q", sc.Opts.Env["SCION_METADATA_PORT"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
	// No SA env vars should be set in block mode
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "" {
		t.Errorf("expected empty SCION_METADATA_SA_EMAIL in block mode, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
}

func TestBuildStartContext_GCPMetadataPassthrough(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Explicit passthrough — should NOT set metadata env vars
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-passthrough",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "passthrough",
			},
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "" {
		t.Errorf("expected no SCION_METADATA_MODE for passthrough, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "" {
		t.Errorf("expected no GCE_METADATA_ROOT for passthrough, got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

func TestBuildStartContext_GCPMetadataExplicitBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-block",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "block",
			},
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

// TestBuildStartContext_GCPMetadataFromResolvedEnv verifies that the start
// path (no Config.GCPIdentity) picks up GCP identity from resolvedEnv
// injected by the hub.  This is the code path hit by "Create & Edit" where
// the agent is provisioned first and then started with updated config.
func TestBuildStartContext_GCPMetadataFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate hub injecting GCP identity via resolvedEnv (start path)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resolved-assign",
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":       "assign",
			"SCION_METADATA_SA_EMAIL":   "sa@proj.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID": "my-project",
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "assign" {
		t.Errorf("expected SCION_METADATA_MODE='assign', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "sa@proj.iam.gserviceaccount.com" {
		t.Errorf("expected SA email from resolvedEnv, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "my-project" {
		t.Errorf("expected project ID from resolvedEnv, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

func TestBuildStartContext_GCPMetadataPassthroughFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate hub injecting passthrough mode via resolvedEnv
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resolved-passthrough",
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
		},
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Passthrough should NOT set metadata server env vars
	if sc.Opts.Env["SCION_METADATA_PORT"] != "" {
		t.Errorf("expected no SCION_METADATA_PORT for passthrough, got %q", sc.Opts.Env["SCION_METADATA_PORT"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}
