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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// In multi-hub mode the per-hub heartbeat filter takes a hub-managed
// project's hub endpoint from the project's .scion settings, never from a
// settings file at the workspace root.
func TestBuildProjectFilterForHub_IgnoresWorkspaceRootSettings(t *testing.T) {
	srv := newTestServer(t)
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a"}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b"}
	srv.hubMu.Unlock()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.yaml"), []byte("hub:\n  endpoint: http://hub-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".scion", "settings.yaml"), []byte("hub:\n  endpoint: http://hub-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.SwapRuntime(&runtime.MockRuntime{
		NameFunc: func() string { return "mock" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{ID: "c1", Name: "a1", Phase: "running", ProjectID: "p1", ProjectPath: root}}, nil
		},
	}, config.RuntimeBuildRecord{})

	if filter := srv.buildProjectFilterForHub("http://hub-a"); filter == nil || filter("p1") {
		t.Error("hub-a filter includes p1; its endpoint must come from .scion/settings.yaml (hub-b)")
	}
	if filter := srv.buildProjectFilterForHub("http://hub-b"); filter == nil || !filter("p1") {
		t.Error("hub-b filter excludes p1")
	}
}

// A hub-managed workspace root without a .scion entry: neither its own
// settings file nor its own harness-configs/ directory is read (no
// project tier), including the env-gather harness-config name probe.
func TestHubWorkspaceWithoutScion_NoRootConfigRead(t *testing.T) {
	// newTestServer sets its own HOME; this test's HOME is set after it.
	srv := newTestServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".scion", "projects", "proj")
	if err := os.MkdirAll(filepath.Join(root, "harness-configs", "tplx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "harness-configs", "tplx", "config.yaml"), []byte("harness: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.yaml"), []byte("hub:\n  endpoint: https://root.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, want := resolveProjectSettingsDir(root), filepath.Join(root, ".scion"); got != want {
		t.Errorf("resolveProjectSettingsDir = %q, want %q", got, want)
	}
	if ep := hubEndpointFromProjectSettings(root); ep != "" {
		t.Errorf("hub endpoint %q read from the workspace root", ep)
	}
	if tier := harnessConfigProjectTier(root); tier != "" {
		t.Errorf("harness-config project tier = %q, want none", tier)
	}
	req := CreateAgentRequest{ProjectPath: root, Config: &CreateAgentConfig{Template: "tplx"}}
	if name := srv.resolveHarnessConfigForEnvGather(req, nil); name == "tplx" {
		t.Error("the env-gather probe took the template name from the workspace root's harness-configs/")
	}

	// With a .scion directory, the project tier is that directory.
	if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, ".scion"))
	if err != nil {
		t.Fatal(err)
	}
	if tier := harnessConfigProjectTier(root); tier != want {
		t.Errorf("harness-config project tier = %q, want %q", tier, want)
	}
	if name := srv.resolveHarnessConfigForEnvGather(req, nil); name == "tplx" {
		t.Error("the env-gather probe took the template name from the workspace root's harness-configs/ (with .scion)")
	}
}
