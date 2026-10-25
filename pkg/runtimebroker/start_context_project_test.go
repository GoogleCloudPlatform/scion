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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

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
		Operation:   opCreate,
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
	home := t.TempDir()
	t.Setenv("HOME", home)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "my-project",
		ProjectID:   "aabbccdd-1234-5678-9012-abcdef123456",
		Operation:   opCreate,
		// A current hub sends the hub-managed project path as the
		// workspace for a shared non-git project (see
		// ambiguousNonGitWorkspace).
		Config: &CreateAgentConfig{Workspace: filepath.Join(home, ".scion", "projects", "my-project")},
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

// TestBuildStartContext_NeverFallsBackToLegacyGrovesDir is the negative test
// for the deleted groves/ fallback: even when a legacy
// ~/.scion/groves/<slug> directory holds real content (a migrator-conflict
// leftover, or a project that predates the migrator ever running) and
// ~/.scion/projects/<slug> holds only infrastructure, buildStartContext must
// always resolve to the canonical projects/ path.
func TestBuildStartContext_NeverFallsBackToLegacyGrovesDir(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	home := t.TempDir()
	t.Setenv("HOME", home)

	slug := "no-legacy-fallback"
	projectsDir := filepath.Join(home, ".scion", "projects", slug)
	if err := os.MkdirAll(filepath.Join(projectsDir, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	grovesDir := filepath.Join(home, ".scion", "groves", slug)
	if err := os.MkdirAll(grovesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grovesDir, "README.md"), []byte("# workspace"), 0644); err != nil {
		t.Fatal(err)
	}

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: slug,
		ProjectID:   "aabbccdd-1234-5678-9012-abcdef123456",
		Operation:   opCreate,
		Config:      &CreateAgentConfig{Workspace: projectsDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.ProjectPath != projectsDir {
		t.Errorf("ProjectPath = %q, want %q (must never fall back to the legacy groves dir)", sc.Opts.ProjectPath, projectsDir)
	}
}

// TestBuildStartContext_GlobalDirFailureMessageOmitsRawError proves that a
// config.GetGlobalDir failure (ProjectSlug set, HOME unresolvable) returns a
// startContextError whose Message is a fixed string, not "...: " + the raw
// os.UserHomeDir error text — a 4xx Status writes Message verbatim to the
// HTTP response body (writeStartContextError), so it must never be built by
// string-concatenating a wrapped error even when this particular failure
// isn't itself a 4xx.
func TestBuildStartContext_GlobalDirFailureMessageOmitsRawError(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "my-project",
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatal("buildStartContext() with unresolvable HOME: expected an error, got nil")
	}
	sce, ok := err.(*startContextError)
	if !ok {
		t.Fatalf("err = %T, want *startContextError", err)
	}
	if strings.Contains(sce.Message, "$HOME") || strings.Contains(sce.Message, "defined") {
		t.Errorf("Message = %q, embeds the raw os.UserHomeDir error text", sce.Message)
	}
	if sce.Message != "Failed to resolve the global config directory" {
		t.Errorf("Message = %q, want the fixed string", sce.Message)
	}
	if sce.OriginalErr == nil {
		t.Error("OriginalErr = nil, want the underlying error preserved for logging/span")
	}
}

// TestBuildStartContext_HubEndpointResolutionFailureMessageOmitsRawError
// proves the same for resolveEffectiveHubEndpoint's failure path (triggered
// here via cloudrun-sandbox with no configured hub listen port): Message
// must be a fixed string, not the wrapped error's own text.
func TestBuildStartContext_HubEndpointResolutionFailureMessageOmitsRawError(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 0
	srv := newTestServerForStartContextRuntime(t, cfg, "cloudrun-sandbox")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox-no-port",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatal("buildStartContext() with HubListenPort=0 on cloudrun-sandbox: expected an error, got nil")
	}
	sce, ok := err.(*startContextError)
	if !ok {
		t.Fatalf("err = %T, want *startContextError", err)
	}
	if strings.Contains(sce.Message, "listen port") {
		t.Errorf("Message = %q, embeds the raw hub-endpoint-resolution error text", sce.Message)
	}
	if sce.Message != "Failed to resolve the hub endpoint" {
		t.Errorf("Message = %q, want the fixed string", sce.Message)
	}
	if sce.OriginalErr == nil {
		t.Error("OriginalErr = nil, want the underlying error preserved for logging/span")
	}
}

func TestBuildStartContext_HubManagedProjectPreservesExistingProjectID(t *testing.T) {
	t.Run("preserves when external config dir exists", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a directory with an existing project-id (git project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		scionDir := filepath.Join(projectPath, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		if err := config.WriteProjectID(scionDir, existingID); err != nil {
			t.Fatal(err)
		}

		// Create the external config dir so it looks like a live project (not stale).
		extDir, err := config.GetGitProjectExternalConfigDir(scionDir)
		if err != nil {
			t.Fatalf("failed to get external config dir: %v", err)
		}
		if err := os.MkdirAll(extDir, 0755); err != nil {
			t.Fatalf("failed to create external config dir: %v", err)
		}

		_, err = srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   "new-id-from-hub",
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify existing project-id was NOT overwritten (external dir exists → not stale)
		projectID, err := config.ReadProjectID(scionDir)
		if err != nil {
			t.Fatalf("failed to read project-id: %v", err)
		}
		if projectID != existingID {
			t.Errorf("expected existing project-id %q to be preserved, got %q", existingID, projectID)
		}
	})

	t.Run("overwrites when external config dir missing (stale)", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a directory with an existing project-id (git project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		scionDir := filepath.Join(projectPath, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		if err := config.WriteProjectID(scionDir, existingID); err != nil {
			t.Fatal(err)
		}
		// Do NOT create the external config dir → simulates project deletion.

		newID := "new-id-from-hub"
		_, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   newID,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify stale project-id was overwritten with the hub's new ID.
		projectID, err := config.ReadProjectID(scionDir)
		if err != nil {
			t.Fatalf("failed to read project-id: %v", err)
		}
		if projectID != newID {
			t.Errorf("expected stale project-id to be overwritten with %q, got %q", newID, projectID)
		}
	})
}

func TestBuildStartContext_HubManagedProjectPreservesExistingMarker(t *testing.T) {
	t.Run("preserves when external config dir exists", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a marker file (hub-managed project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		if err := os.MkdirAll(projectPath, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		scionPath := filepath.Join(projectPath, ".scion")
		existingMarker := &config.ProjectMarker{
			ProjectID:   existingID,
			ProjectName: "existing-project",
			ProjectSlug: "existing-project",
		}
		if err := config.WriteProjectMarker(scionPath, existingMarker); err != nil {
			t.Fatal(err)
		}

		// Create the external config dir so it looks like a live project (not stale).
		extPath, err := existingMarker.ExternalProjectPath()
		if err != nil {
			t.Fatalf("failed to get external project path: %v", err)
		}
		if err := os.MkdirAll(extPath, 0755); err != nil {
			t.Fatalf("failed to create external config dir: %v", err)
		}

		_, err = srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   "new-id-from-hub",
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify existing marker was NOT overwritten (external dir exists → not stale)
		marker, err := config.ReadProjectMarker(scionPath)
		if err != nil {
			t.Fatalf("failed to read marker: %v", err)
		}
		if marker.ProjectID != existingID {
			t.Errorf("expected existing project-id %q to be preserved, got %q", existingID, marker.ProjectID)
		}
	})

	t.Run("overwrites when external config dir missing (stale)", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a marker file (hub-managed project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		if err := os.MkdirAll(projectPath, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		scionPath := filepath.Join(projectPath, ".scion")
		if err := config.WriteProjectMarker(scionPath, &config.ProjectMarker{
			ProjectID:   existingID,
			ProjectName: "existing-project",
			ProjectSlug: "existing-project",
		}); err != nil {
			t.Fatal(err)
		}
		// Do NOT create the external config dir → simulates project deletion.

		newID := "new-id-from-hub"
		_, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   newID,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify stale marker was overwritten with the hub's new ID.
		marker, err := config.ReadProjectMarker(scionPath)
		if err != nil {
			t.Fatalf("failed to read marker: %v", err)
		}
		if marker.ProjectID != newID {
			t.Errorf("expected stale marker project-id to be overwritten with %q, got %q", newID, marker.ProjectID)
		}
	})
}

// TestBuildStartContext_LinkedGitProjectUpdatesStaleProjectID verifies that for
// linked git projects (where ProjectSlug is empty but ProjectID and ProjectPath
// are set), a stale on-disk project-id is overwritten when the external config
// dir was cleaned up. This is the primary regression test for miller79/scion#28.
func TestBuildStartContext_LinkedGitProjectUpdatesStaleProjectID(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate a linked git project workspace with a stale .scion/project-id.
	projectPath := filepath.Join(t.TempDir(), "my-repo")
	scionDir := filepath.Join(projectPath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := config.WriteProjectID(scionDir, staleID); err != nil {
		t.Fatal(err)
	}
	// No external config dir created → the old project was deleted.

	newID := "11111111-2222-3333-4444-555555555555"
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectPath: projectPath,
		ProjectID:   newID,
		// ProjectSlug intentionally empty — linked git project path
		// (hub dispatcher omits slug when provider has LocalPath).
		Operation: opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the stale project-id was overwritten with the hub's new ID.
	projectID, err := config.ReadProjectID(scionDir)
	if err != nil {
		t.Fatalf("failed to read project-id: %v", err)
	}
	if projectID != newID {
		t.Errorf("expected stale project-id to be overwritten with %q, got %q", newID, projectID)
	}
}
