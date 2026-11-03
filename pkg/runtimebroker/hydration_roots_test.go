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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// The broker registers its template cache as a hydration root, and no
// registered root is (or contains) a project or agent directory: an agent
// home, which the agent can write, is never accepted as an absolute template
// directory.
func TestTemplateHydrationRootsAreBrokerCaches(t *testing.T) {
	prev := config.SetTemplateHydrationRoots()
	t.Cleanup(func() { config.SetTemplateHydrationRoots(prev...) })
	t.Setenv("HOME", t.TempDir())

	base := t.TempDir()
	cacheDir := filepath.Join(base, "broker", "cache", "templates")
	cache, err := templatecache.New(cacheDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	srv.SetTemplateCache(cache)

	projectDir := filepath.Join(base, "project", config.DotScion)
	agentHome := config.GetAgentHomePath(projectDir, "worker")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}

	roots := config.TemplateHydrationRoots()
	found := false
	for _, r := range roots {
		if r == cache.Dir() {
			found = true
		}
		for _, p := range []string{projectDir, agentHome} {
			if rel, err := filepath.Rel(r, p); err == nil && filepath.IsLocal(rel) {
				t.Errorf("hydration root %s contains %s", r, p)
			}
			if rel, err := filepath.Rel(p, r); err == nil && (rel == "." || filepath.IsLocal(rel)) {
				t.Errorf("hydration root %s lies inside %s", r, p)
			}
		}
	}
	if !found {
		t.Fatalf("roots %v do not include the template cache %s", roots, cache.Dir())
	}
	for _, dir := range []string{agentHome, filepath.Join(projectDir, "templates")} {
		if _, err := config.FindTemplateInProjectPath(dir, projectDir); !errors.Is(err, config.ErrTemplateRefNotAllowed) {
			t.Errorf("FindTemplateInProjectPath(%s) error = %v, want ErrTemplateRefNotAllowed", dir, err)
		}
	}
}

// A co-located local storage backend contributes only its template
// directories as hydration roots: a template stored there is accepted, and an
// agent workspace upload in the same backend is refused.
func TestColocatedStorageHydrationRootsAreTemplateDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const hubID = "hub-1"
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, LocalPath: t.TempDir(), Bucket: "local"})
	if err != nil {
		t.Fatal(err)
	}
	mkTemplate := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("description: t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	templates := []string{
		stor.ObjectFSPath(storage.TemplateStoragePath(hubID, "global", "", "web")),
		stor.ObjectFSPath(storage.TemplateStoragePath("", "project", "p-1", "web")),
	}
	workspaces := []string{
		stor.ObjectFSPath(storage.WorkspaceStoragePath(hubID, "p-1", "a-1")),
		stor.ObjectFSPath(storage.WorkspaceStoragePath("", "p-1", "a-1")),
	}
	for _, d := range append(append([]string{}, templates...), workspaces...) {
		mkTemplate(d)
	}

	prev := config.SetTemplateHydrationRoots()
	t.Cleanup(func() { config.SetTemplateHydrationRoots(prev...) })
	srv := newTestServer(t)
	srv.config.ColocatedHubID = hubID
	srv.registerColocatedTemplateRoots(stor)

	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	for _, d := range templates {
		if tpl, err := config.FindTemplateInProjectPath(d, projectDir); err != nil || tpl.Path != d {
			t.Errorf("stored template %s: %+v, %v", d, tpl, err)
		}
	}
	for _, d := range workspaces {
		for _, r := range config.TemplateHydrationRoots() {
			if rel, err := filepath.Rel(r, d); err == nil && filepath.IsLocal(rel) {
				t.Errorf("workspace %s lies under hydration root %s", d, r)
			}
		}
		if _, err := config.FindTemplateInProjectPath(d, projectDir); !errors.Is(err, config.ErrTemplateRefNotAllowed) {
			t.Errorf("FindTemplateInProjectPath(workspace %s) error = %v, want ErrTemplateRefNotAllowed", d, err)
		}
	}
}

// allowHydrationRoot registers dir as a template hydration root, as
// initHubConnections registers a co-located local storage directory, for a
// test that attaches a local storage backend by hand (the template directory
// of that backend, not its whole root). The previous roots are
// restored when the test ends.
func allowHydrationRoot(t *testing.T, dir string) {
	t.Helper()
	prev := config.TemplateHydrationRoots()
	config.AddTemplateHydrationRoot(dir)
	t.Cleanup(func() { config.SetTemplateHydrationRoots(prev...) })
}
