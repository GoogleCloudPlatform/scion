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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func linkTestContext() *CLIContext {
	c := newCLIContext()
	c.Hubs = map[string]*HubEntry{
		"local": {Endpoint: "http://127.0.0.1:8080"},
		"prod":  {Endpoint: "https://hub.example.com"},
	}
	return c
}

func TestSetLinkEntryAddsAndLooksUp(t *testing.T) {
	c := linkTestContext()
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	if err := c.SetLinkEntry("prod", KnownProject{ID: "p1", Slug: "foo", Name: "Foo"}, "/src/foo", now); err != nil {
		t.Fatalf("SetLinkEntry: %v", err)
	}
	p, h := c.LinkEntryFor("/src/foo")
	if p == nil || h == nil {
		t.Fatal("LinkEntryFor found no link entry")
	}
	if p.Hub != "prod" || p.ID != "p1" || p.Source != KnownProjectSourceLink || p.LocalPath != "/src/foo" || !p.LastUsed.Equal(now) {
		t.Errorf("link entry = %+v", *p)
	}
	if h.Endpoint != "https://hub.example.com" {
		t.Errorf("hub endpoint = %q", h.Endpoint)
	}
	if p, _ := c.LinkEntryFor("/src/other"); p != nil {
		t.Errorf("LinkEntryFor(other path) = %+v, want none", *p)
	}
}

func TestSetLinkEntryReplacesTheSamePath(t *testing.T) {
	c := linkTestContext()
	now := time.Now()
	if err := c.SetLinkEntry("local", KnownProject{ID: "p1"}, "/src/foo", now); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLinkEntry("prod", KnownProject{ID: "p2"}, "/src/foo", now); err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 1 {
		t.Fatalf("projects = %+v, want one link entry", c.Projects)
	}
	if p, _ := c.LinkEntryFor("/src/foo"); p == nil || p.Hub != "prod" || p.ID != "p2" {
		t.Errorf("link entry after re-link = %+v", p)
	}
}

func TestSetLinkEntryAdoptsAKnownProject(t *testing.T) {
	c := linkTestContext()
	c.Projects = []KnownProject{{Hub: "prod", ID: "p1", Slug: "foo", Name: "Foo", Source: KnownProjectSourceUse}}
	if err := c.SetLinkEntry("prod", KnownProject{ID: "p1"}, "/src/foo", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 1 {
		t.Fatalf("projects = %+v, want the existing entry reused", c.Projects)
	}
	got := c.Projects[0]
	if got.Source != KnownProjectSourceLink || got.LocalPath != "/src/foo" || got.Slug != "foo" || got.Name != "Foo" {
		t.Errorf("entry = %+v", got)
	}
}

func TestSetLinkEntryKeepsOtherDirectoriesOfTheSameProject(t *testing.T) {
	c := linkTestContext()
	now := time.Now()
	if err := c.SetLinkEntry("prod", KnownProject{ID: "p1"}, "/src/a", now); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLinkEntry("prod", KnownProject{ID: "p1"}, "/src/b", now); err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 2 {
		t.Fatalf("projects = %+v, want one link entry per path", c.Projects)
	}
	for _, path := range []string{"/src/a", "/src/b"} {
		if p, _ := c.LinkEntryFor(path); p == nil {
			t.Errorf("no link entry for %s", path)
		}
	}
}

func TestSetLinkEntryErrors(t *testing.T) {
	c := linkTestContext()
	if err := c.SetLinkEntry("nope", KnownProject{ID: "p1"}, "/src/foo", time.Now()); err == nil {
		t.Error("unknown hub: want an error")
	}
	if err := c.SetLinkEntry("prod", KnownProject{}, "/src/foo", time.Now()); err == nil {
		t.Error("no project ID: want an error")
	}
	if err := c.SetLinkEntry("prod", KnownProject{ID: "p1"}, "", time.Now()); err == nil {
		t.Error("no local path: want an error")
	}
}

func TestRemoveLinkEntryKeepsOtherSources(t *testing.T) {
	c := linkTestContext()
	c.Projects = []KnownProject{
		{Hub: "prod", ID: "p1", Source: KnownProjectSourceUse},
		{Hub: "prod", ID: "p2", Source: KnownProjectSourceLink, LocalPath: "/src/foo"},
		{Hub: "prod", ID: "p3", Source: KnownProjectSourceRefresh},
	}
	if !c.RemoveLinkEntry("/src/foo") {
		t.Fatal("RemoveLinkEntry reported nothing removed")
	}
	if c.RemoveLinkEntry("/src/foo") {
		t.Error("second RemoveLinkEntry reported a removal")
	}
	if len(c.Projects) != 2 || c.Projects[0].ID != "p1" || c.Projects[1].ID != "p3" {
		t.Errorf("projects = %+v", c.Projects)
	}
}

func TestLinkEntryForSkipsAMissingHub(t *testing.T) {
	c := linkTestContext()
	c.Projects = []KnownProject{{Hub: "gone", ID: "p1", Source: KnownProjectSourceLink, LocalPath: "/src/foo"}}
	if p, h := c.LinkEntryFor("/src/foo"); p != nil || h != nil {
		t.Errorf("LinkEntryFor = %+v, %+v; want none for a hub without an entry", p, h)
	}
}

func TestLinkEntryRoundTrip(t *testing.T) {
	cliContextTestHome(t)
	err := UpdateCLIContext(func(c *CLIContext) error {
		if _, created := c.EnsureHub("https://hub.example.com", "prod"); !created {
			t.Error("EnsureHub did not create the entry")
		}
		return c.SetLinkEntry("prod", KnownProject{ID: "p1", Slug: "foo"}, "/src/foo", time.Now())
	})
	if err != nil {
		t.Fatalf("UpdateCLIContext: %v", err)
	}
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatalf("LoadCLIContext: %v", err)
	}
	if p, _ := c.LinkEntryFor("/src/foo"); p == nil || p.ID != "p1" {
		t.Errorf("link entry after reload = %+v", p)
	}
}

func TestLinkPathForProject(t *testing.T) {
	home := cliContextTestHome(t)
	repo := filepath.Join(home, "src", "repo")
	if err := os.MkdirAll(filepath.Join(repo, DotScion), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, _ = filepath.EvalSymlinks(repo)
	if got := LinkPathForProject(filepath.Join(repo, DotScion)); got != repo {
		t.Errorf("LinkPathForProject(repo/.scion) = %q, want %q", got, repo)
	}
	if got := LinkPathForProject(filepath.Join(repo, DotScion) + string(filepath.Separator)); got != repo {
		t.Errorf("trailing separator: got %q, want %q", got, repo)
	}

	// A symlinked path resolves to the same link path.
	link := filepath.Join(home, "repolink")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if got := LinkPathForProject(filepath.Join(link, DotScion)); got != repo {
		t.Errorf("via symlink: got %q, want %q", got, repo)
	}

	if got := LinkPathForProject(""); got != "" {
		t.Errorf("LinkPathForProject(\"\") = %q", got)
	}
}

func TestLinkPathForProjectExternalSettings(t *testing.T) {
	home := cliContextTestHome(t)
	workspace := filepath.Join(home, "work", "proj")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, _ = filepath.EvalSymlinks(workspace)
	ext := filepath.Join(home, DotScion, ProjectConfigsDir, "proj__abcd", DotScion)
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, "settings.yaml"), []byte("schema_version: \"1\"\nworkspace_path: "+workspace+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LinkPathForProject(ext); got != workspace {
		t.Errorf("LinkPathForProject(external) = %q, want the workspace %q", got, workspace)
	}
}
