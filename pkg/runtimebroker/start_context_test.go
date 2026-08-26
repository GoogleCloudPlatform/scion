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

// TestBuildStartContext_AuthTokenPrecedence verifies the SCION_AUTH_TOKEN
// resolution precedence in buildStartContext step 3:
//  1. in.AgentToken (explicit hub-provided field) wins.
//  2. an existing token from ResolvedEnv (start/resume path) is kept and must
//     NOT be clobbered by the broker's own dev SCION_AUTH_TOKEN. This is the
//     regression case for the resume 401 ("compact JWS format must have three
//     parts").
//  3. the broker dev token is used only when neither of the above is present.
func TestBuildStartContext_AuthTokenPrecedence(t *testing.T) {
	// Valid-looking JWT (three dot-separated parts) minted by the hub.
	const hubToken = "header.payload.signature"
	const devToken = "broker-dev-token"
	const explicitToken = "explicit.agent.token"

	t.Run("resolvedEnv token kept over broker dev token", func(t *testing.T) {
		// Broker has its own (invalid-as-JWT) dev token in the environment.
		t.Setenv("SCION_AUTH_TOKEN", devToken)

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-resume",
			// Hub minted the agent JWT into resolvedEnv on the start/resume path.
			ResolvedEnv: map[string]string{
				"SCION_AUTH_TOKEN": hubToken,
			},
			// AgentToken intentionally empty (start/resume path).
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_AUTH_TOKEN"]; got != hubToken {
			t.Errorf("expected SCION_AUTH_TOKEN to keep hub-minted token %q, got %q (broker dev token must not clobber)", hubToken, got)
		}
	})

	t.Run("explicit AgentToken wins", func(t *testing.T) {
		t.Setenv("SCION_AUTH_TOKEN", devToken)

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:       "agent-create",
			AgentToken: explicitToken,
			ResolvedEnv: map[string]string{
				"SCION_AUTH_TOKEN": hubToken,
			},
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_AUTH_TOKEN"]; got != explicitToken {
			t.Errorf("expected SCION_AUTH_TOKEN=%q (explicit AgentToken wins), got %q", explicitToken, got)
		}
	})

	t.Run("broker dev token used as last resort", func(t *testing.T) {
		t.Setenv("SCION_AUTH_TOKEN", devToken)

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-plain-broker",
			// No AgentToken and no SCION_AUTH_TOKEN in resolvedEnv.
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_AUTH_TOKEN"]; got != devToken {
			t.Errorf("expected SCION_AUTH_TOKEN=%q (broker dev fallback), got %q", devToken, got)
		}
	})
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

// TestBuildStartContext_GCPMetadataUnknownModeRejected covers the fail-open case
// the allow-list exists to close. Before the inversion an unrecognised mode was
// treated as a no-op: no SCION_METADATA_MODE, no GCE_METADATA_HOST redirect, and
// therefore a container talking to the real GCE metadata server. The failure was
// silent, which is what made it dangerous — a typo in a mode string downgraded
// the agent to unprotected rather than refusing to start it.
//
// The empty-string case matters most. It is not a typo an operator makes; it is
// what a non-nil GCPIdentity with an unset MetadataMode produces, and it used to
// be indistinguishable from passthrough.
func TestBuildStartContext_GCPMetadataUnknownModeRejected(t *testing.T) {
	modes := map[string]string{
		"typo":            "blocked",
		"unknown value":   "sandbox",
		"empty":           "",
		"wrong case":      "Block",
		"leading space":   " block",
		"prefix of known": "bl",
	}

	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-bad-mode",
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: mode,
					},
				},
				HTTPRequest: r,
			})
			if err == nil {
				t.Fatalf("expected an error for metadata mode %q, got nil (env: %v)", mode, sc.Opts.Env)
			}
			// The start must fail rather than proceed with a partial or absent
			// redirect, so there is no context to inspect at all.
			if sc != nil {
				t.Errorf("expected nil startContext alongside the error, got %+v", sc)
			}
			if !strings.Contains(err.Error(), "Invalid GCP metadata mode") {
				t.Errorf("expected an invalid-mode error, got %q", err.Error())
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataUnknownModeFromResolvedEnv checks the second
// input path. The mode can also arrive as hub-injected resolvedEnv on the start
// path, where there is no CreateAgentConfig at all, so the allow-list has to
// cover it too — a broker that validated only the create path would still accept
// an unknown mode from a newer or misbehaving hub.
func TestBuildStartContext_GCPMetadataUnknownModeFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-bad-env-mode",
		ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "blocked"},
		HTTPRequest: r,
	})
	if err == nil {
		t.Fatalf("expected an error for hub-injected mode 'blocked', got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "Invalid GCP metadata mode") {
		t.Errorf("expected an invalid-mode error, got %q", err.Error())
	}
}

// TestBuildStartContext_GCPMetadataModeFromResolvedEnvStillAccepted guards the
// inversion against over-reach: the start path must keep working for the modes
// the hub legitimately injects.
func TestBuildStartContext_GCPMetadataModeFromResolvedEnvStillAccepted(t *testing.T) {
	for _, mode := range []string{"assign", "block"} {
		t.Run(mode, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-env-mode",
				ResolvedEnv: map[string]string{"SCION_METADATA_MODE": mode},
				HTTPRequest: r,
			})
			if err != nil {
				t.Fatalf("expected mode %q to be accepted, got %v", mode, err)
			}
			if sc.Opts.Env["SCION_METADATA_MODE"] != mode {
				t.Errorf("expected SCION_METADATA_MODE=%q, got %q", mode, sc.Opts.Env["SCION_METADATA_MODE"])
			}
			if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected the metadata redirect to be set, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
			}
		})
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

// newTestServerWithRuntime creates a test server like newTestServerForStartContext
// but with a custom runtime name.
func newTestServerWithRuntime(t *testing.T, cfg ServerConfig, runtimeName string) *Server {
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
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
	}
	return New(cfg, mgr, rt)
}

// TestBuildStartContext_CloudrunSandboxHubEndpoint verifies hub endpoint
// behaviour for the cloudrun-sandbox runtime. In CI (no link-local interface),
// the start must fail rather than fall back to the public IAP URL — a 302
// from the IAP edge is exactly the failure that this fix prevents.
//
// When a link-local address IS available (real Cloud Run Instance), the
// endpoint must be http://<link-local>:<port> with the port read from the
// broker's own hub endpoint config.
func TestBuildStartContext_CloudrunSandboxHubEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 8080
	srv := newTestServerWithRuntime(t, cfg, "cloudrun-sandbox")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox",
		HTTPRequest: r,
	})

	if err != nil {
		// Expected in CI: no link-local address → start fails.
		// Verify it is the right error and not some other failure.
		if !strings.Contains(err.Error(), "hub endpoint") && !strings.Contains(err.Error(), "link-local") {
			t.Fatalf("expected hub-endpoint/link-local error for cloudrun-sandbox, got: %v", err)
		}
		return
	}

	// If we get here, a link-local address was found (real Instance).
	ep := sc.Opts.Env["SCION_HUB_ENDPOINT"]

	// Guard: SCION_HUB_ENDPOINT must never contain a run.app URL for
	// cloudrun-sandbox — that would route through IAP, which the sandbox
	// cannot authenticate against.
	if strings.Contains(ep, "run.app") {
		t.Fatalf("SCION_HUB_ENDPOINT must never be a run.app URL for cloudrun-sandbox, got %q", ep)
	}

	// Must be http (not https) on the link-local address.
	if !strings.HasPrefix(ep, "http://169.254.") {
		t.Fatalf("expected http://169.254.x.x:<port>, got %q", ep)
	}

	// Port must come from the broker config, not hardcoded.
	if !strings.HasSuffix(ep, ":8080") {
		t.Fatalf("expected port 8080 from broker hub endpoint, got %q", ep)
	}

	// Metadata vars must still be localhost — emulator runs inside sandbox.
	host := sc.Opts.Env["GCE_METADATA_HOST"]
	root := sc.Opts.Env["GCE_METADATA_ROOT"]
	if host != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", host)
	}
	if root != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", root)
	}
}

// TestBuildStartContext_CloudrunSandboxHubNeverRunApp is a durable guard:
// assert the negative so the test survives refactoring and catches the
// shipped defect pattern. If buildStartContext succeeds for cloudrun-sandbox,
// the hub endpoint must NEVER be a public IAP-fronted URL.
func TestBuildStartContext_CloudrunSandboxHubNeverRunApp(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 8080
	srv := newTestServerWithRuntime(t, cfg, "cloudrun-sandbox")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate a hub-dispatched agent whose resolvedEnv carries the public URL.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox-iap",
		ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": "https://my-instance-xyz.run.app"},
		HTTPRequest: r,
	})
	if err != nil {
		// In CI this fails (no link-local) — that is correct.
		return
	}
	ep := sc.Opts.Env["SCION_HUB_ENDPOINT"]
	if strings.Contains(ep, "run.app") {
		t.Fatalf("cloudrun-sandbox SCION_HUB_ENDPOINT must never contain run.app, got %q", ep)
	}
}

// TestBuildStartContext_GCPMetadataBothVarsAlwaysMatch guards the invariant
// that GCE_METADATA_HOST and GCE_METADATA_ROOT are always set to the same
// value. A mismatch means gcloud (ROOT) and language SDKs (HOST) would talk
// to different servers. Only tests non-cloudrun-sandbox runtimes since
// cloudrun-sandbox may fail in CI (no link-local).
func TestBuildStartContext_GCPMetadataBothVarsAlwaysMatch(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerWithRuntime(t, cfg, "mock")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-both-vars",
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	host := sc.Opts.Env["GCE_METADATA_HOST"]
	root := sc.Opts.Env["GCE_METADATA_ROOT"]
	if host != root {
		t.Errorf("GCE_METADATA_HOST=%q GCE_METADATA_ROOT=%q — must match", host, root)
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

func TestResolveWorktreeProvision_KubernetesNodeLocal_Rejected(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		RuntimeName: "kubernetes",
		eligibilityOverride: func() (bool, string) {
			return true, ""
		},
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false for Kubernetes without NFS backend")
	}
	if !strings.Contains(result.Reason, "Kubernetes") {
		t.Errorf("expected reason to mention Kubernetes, got %q", result.Reason)
	}
	if !strings.Contains(result.Reason, "NFS") {
		t.Errorf("expected reason to mention NFS requirement, got %q", result.Reason)
	}
	if result.ProvisionInput.ProjectID != "" {
		t.Error("expected empty ProvisionInput when rejected")
	}
}

func TestResolveWorktreeProvision_DockerRuntime_NotRejected(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		RuntimeName: "docker",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true for Docker runtime, got false (reason: %s)", result.Reason)
	}
}

func TestResolveWorktreeProvision_EmptyRuntime_NotRejected(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		AgentID:     "agent-1",
		RuntimeName: "",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true for empty RuntimeName, got false (reason: %s)", result.Reason)
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

// TestTryProvisionWorktree_JoinResolvesSharedPath verifies that when agent-b
// is provisioned with --branch pointing to an already-checked-out branch
// (agent-a's), provisioning succeeds as a JOIN and opts.Workspace is set to
// agent-a's worktree path (not WorktreePath(base, agent-b)).
func TestTryProvisionWorktree_JoinResolvesSharedPath(t *testing.T) {
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

	// Set up the shared base + agent-a's worktree on branch "agent-a".
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
	agentAWt := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(agentAWt); err != nil {
		t.Fatalf("agent-a worktree missing after setup: %v", err)
	}

	// Provision agent-b with --branch "agent-a" → should JOIN, not fail.
	opts := &api.StartOptions{}
	ok := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-b", AgentID: "agent-b",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc, Branch: "agent-a"},
	}, opts, map[string]string{})

	if !ok {
		t.Fatal("expected JOIN to succeed, got ok=false (fell back to clone-per-agent)")
	}

	// opts.Workspace must point to agent-a's worktree (the shared path).
	if opts.Workspace != agentAWt {
		t.Errorf("opts.Workspace = %q, want %q (agent-a's worktree)", opts.Workspace, agentAWt)
	}

	// No separate worktree created for agent-b.
	agentBWt := provision.WorktreePath(base, "agent-b")
	if _, err := os.Stat(agentBWt); !os.IsNotExist(err) {
		t.Errorf("agent-b should NOT have its own worktree, stat err=%v", err)
	}

	// Both agents registered as sharers.
	sharers, wtPath, err := provision.ListSharers(base, "agent-a")
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if wtPath != agentAWt {
		t.Errorf("registry worktreePath = %q, want %q", wtPath, agentAWt)
	}
	if len(sharers) != 2 {
		t.Fatalf("expected 2 sharers, got %d: %v", len(sharers), sharers)
	}

	// Shared base and agent-a worktree are intact.
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Errorf("shared base .git was destroyed: %v", err)
	}
	if _, err := os.Stat(agentAWt); err != nil {
		t.Errorf("agent-a worktree was destroyed: %v", err)
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

func TestBuildStartContext_NoAuth(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	secrets := []api.ResolvedSecret{
		{Name: "CLAUDE_AUTH", Type: "file", Value: "secret-data", Target: "~/.claude/.credentials.json"},
		{Name: "API_KEY", Type: "environment", Value: "key-value", Target: "API_KEY"},
	}

	t.Run("NoAuth=true nils out secrets and sets opts.NoAuth", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:            "noauth-agent",
			ResolvedSecrets: secrets,
			NoAuth:          true,
			HTTPRequest:     r,
		})
		if err != nil {
			t.Fatal(err)
		}

		if !sc.Opts.NoAuth {
			t.Error("expected opts.NoAuth to be true")
		}
		if sc.Opts.ResolvedSecrets != nil {
			t.Errorf("expected nil ResolvedSecrets with NoAuth, got %d", len(sc.Opts.ResolvedSecrets))
		}
	})

	t.Run("NoAuth=false passes secrets through", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:            "auth-agent",
			ResolvedSecrets: secrets,
			NoAuth:          false,
			HTTPRequest:     r,
		})
		if err != nil {
			t.Fatal(err)
		}

		if sc.Opts.NoAuth {
			t.Error("expected opts.NoAuth to be false")
		}
		if len(sc.Opts.ResolvedSecrets) != 2 {
			t.Errorf("expected 2 resolved secrets, got %d", len(sc.Opts.ResolvedSecrets))
		}
	})
}

// TestBuildStartContext_WorkspaceMode verifies that SCION_WORKSPACE_MODE is emitted
// with the correct canonical value for each wire label, and defaults to shared-plain.
func TestBuildStartContext_WorkspaceMode(t *testing.T) {
	cases := []struct {
		name      string
		wireLabel string
		wantMode  string
	}{
		{
			name:      "shared wire label",
			wireLabel: store.WorkspaceModeShared,
			wantMode:  "shared-plain",
		},
		{
			name:      "per-agent wire label",
			wireLabel: store.WorkspaceModePerAgent,
			wantMode:  "clone-per-agent",
		},
		{
			name:      "worktree-per-agent wire label",
			wireLabel: store.WorkspaceModeWorktreePerAgent,
			wantMode:  "worktree-per-agent",
		},
		{
			name:      "empty wire label defaults to shared-plain",
			wireLabel: "",
			wantMode:  "shared-plain",
		},
		{
			name:      "unrecognized wire label defaults to shared-plain",
			wireLabel: "unknown-mode",
			wantMode:  "shared-plain",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "agent-1",
				WorkspaceMode: tc.wireLabel,
				HTTPRequest:   r,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := sc.Opts.Env["SCION_WORKSPACE_MODE"]; got != tc.wantMode {
				t.Errorf("SCION_WORKSPACE_MODE: got %q, want %q", got, tc.wantMode)
			}
		})
	}
}

// TestBuildStartContext_WorkspaceMode_StartPathFallback verifies that on the
// start/restart path (WorkspaceMode==""), a hub-injected SCION_WORKSPACE_MODE in
// resolvedEnv is propagated to the container env, and the broker-side code does
// not overwrite it with the shared-plain default.
func TestBuildStartContext_WorkspaceMode_StartPathFallback(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resume",
		// Hub injects canonical value via resolvedEnv on start path.
		ResolvedEnv: map[string]string{
			"SCION_WORKSPACE_MODE": "clone-per-agent",
			"SCION_WORKSPACE_GIT":  "true",
		},
		// WorkspaceMode intentionally empty (start/restart path).
		HTTPRequest: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := sc.Opts.Env["SCION_WORKSPACE_MODE"]; got != "clone-per-agent" {
		t.Errorf("SCION_WORKSPACE_MODE: got %q, want %q", got, "clone-per-agent")
	}
	if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
		t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q", got, "true")
	}
}

// TestBuildStartContext_WorkspaceGit verifies that SCION_WORKSPACE_GIT is emitted
// correctly based on the provisioning path.
//
// Coverage note: the highest-priority path — worktreeProvisioned=true (from
// tryProvisionWorktree in start_context.go) — is not covered here because it
// requires a real host-side git repository setup that is outside unit test scope.
// The code path is simple (isGitWorkspace := worktreeProvisioned || ...) and
// covered by the worktree integration tests. The remaining three priority levels
// (GitClone config, on-disk git dir, hub-injected resolvedEnv) are tested below.
func TestBuildStartContext_WorkspaceGit(t *testing.T) {
	t.Run("git clone config implies git workspace", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-clone",
			Config: &CreateAgentConfig{
				GitClone: &api.GitCloneConfig{
					URL:    "https://github.com/example/repo.git",
					Branch: "main",
				},
			},
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
			t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q", got, "true")
		}
	})

	t.Run("no git clone and no workspace implies absent git var", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-plain",
			Config:      &CreateAgentConfig{},
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if v, present := sc.Opts.Env["SCION_WORKSPACE_GIT"]; present {
			t.Errorf("SCION_WORKSPACE_GIT should be absent for non-git workspace, got %q", v)
		}
	})

	t.Run("start path hub-injected SCION_WORKSPACE_GIT propagates", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-start",
			ResolvedEnv: map[string]string{
				"SCION_WORKSPACE_GIT": "true",
			},
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
			t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q", got, "true")
		}
	})

	t.Run("shared-plain git workspace on disk", func(t *testing.T) {
		// Create a temporary git repo to simulate a shared-plain git workspace
		gitDir := t.TempDir()
		initCmd := exec.Command("git", "init", gitDir)
		if out, err := initCmd.CombinedOutput(); err != nil {
			t.Skipf("git init failed: %s", string(out))
		}

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-shared-git",
			Config: &CreateAgentConfig{
				Workspace: gitDir,
			},
			HTTPRequest: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
			t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q (shared git workspace on disk should set it)", got, "true")
		}
	})
}
