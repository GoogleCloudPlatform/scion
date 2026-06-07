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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
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
		ProjectID:   "grove-1",
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
		Name:        "agent-1",
		ProjectPath: "/some/path",
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
	// Git clone mode should clear workspace but preserve project path
	// so ProvisionAgent can resolve the correct agent directory.
	if sc.Opts.Workspace != "" {
		t.Errorf("expected Workspace to be empty in git clone mode, got %q", sc.Opts.Workspace)
	}
	if sc.Opts.ProjectPath != "/some/path" {
		t.Errorf("expected ProjectPath to be preserved in git clone mode, got %q", sc.Opts.ProjectPath)
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

func TestBuildStartContext_HubManagedProjectWritesMarker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate a hub-managed project: ProjectSlug set, ProjectPath pre-resolved
	// (as the createAgent handler does for env-gather), and ProjectID from hub.
	projectsDir := t.TempDir()
	projectPath := filepath.Join(projectsDir, "web-demo")

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "web-demo",
		ProjectPath: projectPath,
		ProjectID:   "6d868c0f-b862-49e0-a44b-3555a3887ee3",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify .scion marker file was created (not a directory)
	scionPath := filepath.Join(projectPath, ".scion")
	if !config.IsProjectMarkerFile(scionPath) {
		t.Fatal(".scion marker file was not created")
	}

	// Verify project-id was written via marker
	marker, err := config.ReadProjectMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read .scion marker: %v", err)
	}
	if marker.ProjectID != "6d868c0f-b862-49e0-a44b-3555a3887ee3" {
		t.Errorf("expected project-id '6d868c0f-b862-49e0-a44b-3555a3887ee3', got %q", marker.ProjectID)
	}
	if marker.ProjectSlug != "web-demo" {
		t.Errorf("expected project-slug 'web-demo', got %q", marker.ProjectSlug)
	}

	// Verify external project-configs directories were created
	extPath, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatalf("failed to get external project path: %v", err)
	}
	if extPath == "" {
		t.Fatal("expected non-empty external project path")
	}
	extAgents := filepath.Join(extPath, "agents")
	if _, err := os.Stat(extAgents); os.IsNotExist(err) {
		t.Fatalf("external agents dir was not created: %s", extAgents)
	}

	// ProjectPath should be passed through to opts
	if sc.Opts.ProjectPath != projectPath {
		t.Errorf("expected ProjectPath %q, got %q", projectPath, sc.Opts.ProjectPath)
	}
}

func TestBuildStartContext_HubManagedProjectSlugResolution(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate: ProjectSlug set, ProjectPath empty (buildStartContext resolves it),
	// ProjectID from hub. This is the path when the handler doesn't pre-resolve.
	t.Setenv("HOME", t.TempDir())

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "my-project",
		ProjectID:   "aabbccdd-1234-5678-9012-abcdef123456",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify project-id was written via marker file
	scionPath := filepath.Join(sc.Opts.ProjectPath, ".scion")
	marker, err := config.ReadProjectMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read .scion marker: %v", err)
	}
	if marker.ProjectID != "aabbccdd-1234-5678-9012-abcdef123456" {
		t.Errorf("expected project-id from hub, got %q", marker.ProjectID)
	}
}

func TestBuildStartContext_HubManagedProjectPreservesExistingProjectID(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Pre-create .scion as a directory with an existing project-id (git project)
	projectPath := filepath.Join(t.TempDir(), "existing-grove")
	scionDir := filepath.Join(projectPath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	existingID := "existing-id-1234-5678"
	if err := config.WriteProjectID(scionDir, existingID); err != nil {
		t.Fatal(err)
	}

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "existing-grove",
		ProjectPath: projectPath,
		ProjectID:   "new-id-from-hub",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify existing project-id was NOT overwritten (directory-based path)
	projectID, err := config.ReadProjectID(scionDir)
	if err != nil {
		t.Fatalf("failed to read project-id: %v", err)
	}
	if projectID != existingID {
		t.Errorf("expected existing project-id %q to be preserved, got %q", existingID, projectID)
	}
}

func TestBuildStartContext_HubManagedProjectPreservesExistingMarker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Pre-create .scion as a marker file (hub-managed project)
	projectPath := filepath.Join(t.TempDir(), "existing-grove")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}
	existingID := "existing-id-1234-5678"
	scionPath := filepath.Join(projectPath, ".scion")
	if err := config.WriteProjectMarker(scionPath, &config.ProjectMarker{
		ProjectID:   existingID,
		ProjectName: "existing-grove",
		ProjectSlug: "existing-grove",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "existing-grove",
		ProjectPath: projectPath,
		ProjectID:   "new-id-from-hub",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify existing marker was NOT overwritten (marker file path)
	marker, err := config.ReadProjectMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read marker: %v", err)
	}
	if marker.ProjectID != existingID {
		t.Errorf("expected existing project-id %q to be preserved, got %q", existingID, marker.ProjectID)
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

// --- resolveWorktreeProvision tests ---

func TestResolveWorktreeProvision_Eligible(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
			Depth:  1,
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
	})

	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		if result.ShouldProvision {
			t.Fatal("expected ShouldProvision=false when git is too old")
		}
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, got false (reason: %s)", result.Reason)
	}

	expectedPath := filepath.Join(projectDir, "workspace", "worktrees", "agent-1")
	if result.WorktreePath != expectedPath {
		t.Errorf("expected WorktreePath=%q, got %q", expectedPath, result.WorktreePath)
	}

	expectedRoot := filepath.Join(projectDir, "workspace")
	if result.ProjectRoot != expectedRoot {
		t.Errorf("expected ProjectRoot=%q, got %q", expectedRoot, result.ProjectRoot)
	}

	pi := result.ProvisionInput
	if pi.Mode != store.SharingModeWorktreePerAgent {
		t.Errorf("expected Mode=worktree-per-agent, got %v", pi.Mode)
	}
	if pi.ProjectID != "proj-1" {
		t.Errorf("expected ProjectID='proj-1', got %q", pi.ProjectID)
	}
	if pi.AgentID != "agent-1" {
		t.Errorf("expected AgentID='agent-1', got %q", pi.AgentID)
	}
	if pi.GitClone == nil || pi.GitClone.URL != "https://github.com/org/repo.git" {
		t.Errorf("expected GitClone.URL set, got %v", pi.GitClone)
	}
	if pi.Locker != nil {
		t.Error("expected Locker=nil for node-local single-broker")
	}
}

func TestResolveWorktreeProvision_BranchOverridesAgentName(t *testing.T) {
	projectDir := t.TempDir()
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   projectDir,
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
		AgentName:     "test-agent",
		Branch:        "feature-branch",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, reason: %s", result.Reason)
	}
	if result.ProvisionInput.AgentName != "feature-branch" {
		t.Errorf("expected AgentName='feature-branch' (from Branch), got %q", result.ProvisionInput.AgentName)
	}
}

func TestResolveWorktreeProvision_WrongMode(t *testing.T) {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false for non-worktree mode")
	}
	if !strings.Contains(result.Reason, "not worktree-per-agent") {
		t.Errorf("expected reason to mention mode mismatch, got %q", result.Reason)
	}
}

func TestResolveWorktreeProvision_NoGitClone(t *testing.T) {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      nil,
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when GitClone is nil")
	}
	if !strings.Contains(result.Reason, "not git-backed") {
		t.Errorf("expected reason to mention non-git, got %q", result.Reason)
	}
}

func TestResolveWorktreeProvision_GitTooOld_Fallback(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
			Depth:  1,
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		eligibilityOverride: func() (bool, string) {
			return false, "git >= 2.47.0 required for worktree-per-agent mode (--relative-paths), found 2.39.0"
		},
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when git is too old")
	}
	if !strings.Contains(result.Reason, "2.47") {
		t.Errorf("expected reason to mention git 2.47 requirement, got %q", result.Reason)
	}
	if result.ProvisionInput.ProjectID != "" {
		t.Error("expected empty ProvisionInput when ineligible")
	}
}

func TestResolveWorktreeProvision_FullCloneDepth(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()
	originalGC := &api.GitCloneConfig{
		URL:    "https://github.com/org/repo.git",
		Branch: "main",
		Depth:  1,
	}

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      originalGC,
		ProjectPath:   projectDir,
		ProjectID:     "proj-1",
		ProjectSlug:   "my-project",
		AgentID:       "agent-1",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, reason: %s", result.Reason)
	}

	if result.ProvisionInput.GitClone.Depth != -1 {
		t.Errorf("expected GitClone.Depth=-1 (full clone), got %d", result.ProvisionInput.GitClone.Depth)
	}

	if originalGC.Depth != 1 {
		t.Errorf("original GitClone.Depth was mutated: got %d, want 1", originalGC.Depth)
	}
}

// initBareRepoWithCommit creates a bare git repo (default branch main) seeded
// with one commit, and returns its path for use as a GitClone URL.
func initBareRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	wc := filepath.Join(dir, "wc")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, strings.TrimSpace(string(out)))
		}
	}
	run("init", "--bare", "-b", "main", bare)
	run("clone", bare, wc)
	if err := os.WriteFile(filepath.Join(wc, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", wc, "add", "-A")
	run("-C", wc, "commit", "-m", "init")
	run("-C", wc, "push", "origin", "main")
	return bare
}

// TestTryProvisionWorktree_FailureDoesNotDeleteSharedBase is the regression for
// the critical review finding on upstream PR #350: when provisioning fails for
// one agent, only that agent's worktree may be removed — never the shared base
// clone (Resolved.HostPath), which holds the common .git and every sibling
// agent's worktree. Deleting it would destroy all other agents' workspaces.
func TestTryProvisionWorktree_FailureDoesNotDeleteSharedBase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: 0}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Set up the shared base + a sibling worktree owning branch "agent-a".
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("setup agent-a: %v", err)
	}
	base := resolved.HostPath
	siblingWt := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Fatalf("base .git missing after setup: %v", err)
	}
	if _, err := os.Stat(siblingWt); err != nil {
		t.Fatalf("sibling worktree missing after setup: %v", err)
	}

	// Provision agent-b with a branch colliding with agent-a's → ensureWorktree
	// fails ("already checked out"), exercising the cleanup-on-failure path.
	opts := &api.StartOptions{}
	ok := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-b", AgentID: "agent-b",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc, Branch: "agent-a"},
	}, opts, map[string]string{})

	if ok {
		t.Fatal("expected provisioning to fail (branch collision) and fall back, got ok=true")
	}

	// CRITICAL: the shared base and the sibling worktree must survive.
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Errorf("shared base .git was destroyed by a failed sibling provision: %v", err)
	}
	if _, err := os.Stat(siblingWt); err != nil {
		t.Errorf("sibling agent-a worktree was destroyed by a failed sibling provision: %v", err)
	}
	// Only the failing agent's partial worktree should be cleaned up.
	if _, err := os.Stat(provision.WorktreePath(base, "agent-b")); !os.IsNotExist(err) {
		t.Errorf("agent-b partial worktree should have been cleaned up, stat err=%v", err)
	}
}

// TestWorktreeWorkspace_RepoRootDerivesToBase validates that the container
// dual-mount inputs resolve correctly for the worktree layout WITHOUT any
// explicit opts.RepoRoot (api.StartOptions has no such field). pkg/agent/run.go
// derives repoRoot from the workspace itself: IsGitRepoDir(worktree) is true
// (git rev-parse --is-inside-work-tree works through the worktree .git pointer
// file), and GetCommonGitDir(worktree) returns the SHARED base .git, so
// repoRoot = filepath.Dir(commonDir) == the base checkout. The worktree then
// sits at <base>/worktrees/<id>, giving a non-".." relative path that triggers
// common.go's .git + worktree dual-mount. (Regression guard for the #350 review
// claim that opts.RepoRoot must be set explicitly.)
func TestWorktreeWorkspace_RepoRootDerivesToBase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: 0}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	base := resolved.HostPath // <projectPath>/workspace — the shared base checkout
	worktree := provision.WorktreePath(base, "agent-a")

	// Replicate pkg/agent/run.go's repoRoot derivation from the workspace.
	if !util.IsGitRepoDir(worktree) {
		t.Fatal("IsGitRepoDir(worktree) = false; run.go would not derive repoRoot from the worktree")
	}
	commonDir, err := util.GetCommonGitDir(worktree)
	if err != nil {
		t.Fatalf("GetCommonGitDir(worktree): %v", err)
	}
	repoRoot := filepath.Dir(commonDir)
	if repoRoot != base {
		t.Errorf("derived repoRoot = %q, want base %q", repoRoot, base)
	}

	// The dual-mount in common.go only fires when rel(repoRoot, workspace) is a
	// non-".." subpath — confirm the worktree is nested inside the base.
	rel, err := filepath.Rel(repoRoot, worktree)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if rel != filepath.Join("worktrees", "agent-a") {
		t.Errorf("rel(repoRoot, worktree) = %q, want %q", rel, filepath.Join("worktrees", "agent-a"))
	}
	if strings.HasPrefix(rel, "..") {
		t.Errorf("rel %q starts with .. — common.go dual-mount would NOT fire", rel)
	}
}
