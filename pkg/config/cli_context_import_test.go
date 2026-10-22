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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// A context file written before the current-hub import existed (the global
// import already recorded, no current hub) gets the global hub as its
// current hub, once.
func TestLoadCLIContextImported_SetsCurrentHubForAnEarlierImport(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	writeCLIContextFile(t, ctxPath, "version: 1\nhubs:\n  prod:\n    endpoint: https://hub.example.com\nimported:\n  global: 2026-10-12T03:00:00Z\n")
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com/\n")

	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentHub != "prod" || !c.CurrentImported() || len(c.Hubs) != 1 {
		t.Fatalf("context = %+v, want prod current and the import recorded", c)
	}
	onDisk, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.CurrentHub != "prod" || !onDisk.CurrentImported() {
		t.Errorf("context.yaml = %+v", onDisk)
	}
}

// The current-hub import runs once: a current hub the user removes later
// is not added back, and nothing becomes current again.
func TestLoadCLIContextImported_CurrentHubOnce(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com\n")
	if _, err := LoadCLIContextImported(importNamer); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCLIContext(func(c *CLIContext) error {
		_, _, err := c.RemoveHub("hub")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := readFileString(t, ctxPath)

	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentHub != "" || len(c.Hubs) != 0 {
		t.Errorf("context = %+v, want no hub: the removed hub came back", c)
	}
	if got := readFileString(t, ctxPath); got != before {
		t.Errorf("context.yaml was rewritten:\n%s", got)
	}
}

// The current-hub import targets the hub the CLI followed until now, the
// global hub.endpoint, even when an earlier binary imported another
// endpoint or the user removed the imported entry: an entry is added for it
// when none has that endpoint, and it becomes the current hub.
func TestLoadCLIContextImported_CurrentHubFollowsTheGlobalEndpoint(t *testing.T) {
	t.Run("another endpoint was imported earlier", func(t *testing.T) {
		ctxPath := cliContextTestHome(t)
		writeCLIContextFile(t, ctxPath, "version: 1\nhubs:\n  old:\n    endpoint: https://old.example.com\nimported:\n  global: 2026-10-12T03:00:00Z\n")
		writeGlobalSettingsFile(t, "hub:\n  endpoint: https://new.example.com\n  transport:\n    mode: iap\n    audience: aud-new\n")

		c, err := LoadCLIContextImported(importNamer)
		if err != nil {
			t.Fatal(err)
		}
		name, h := c.HubByEndpoint("https://new.example.com")
		if h == nil || c.CurrentHub != name {
			t.Fatalf("context = %+v, want the global endpoint's entry current", c)
		}
		if c.Hubs["old"] == nil || !c.CurrentImported() {
			t.Errorf("context = %+v, want the old entry kept and the import recorded", c)
		}
		if want := (&HubTransportConfig{Mode: "iap", Audience: "aud-new"}); !reflect.DeepEqual(h.Transport, want) {
			t.Errorf("transport = %+v, want the global hub.transport %+v", h.Transport, want)
		}
	})
	t.Run("the imported entry was removed", func(t *testing.T) {
		ctxPath := cliContextTestHome(t)
		writeCLIContextFile(t, ctxPath, "version: 1\nimported:\n  global: 2026-10-12T03:00:00Z\n")
		writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com\n")

		c, err := LoadCLIContextImported(importNamer)
		if err != nil {
			t.Fatal(err)
		}
		if c.CurrentHub != "hub" || c.Hubs["hub"] == nil || !c.CurrentImported() {
			t.Errorf("context = %+v, want hub current and the import recorded", c)
		}
	})
}

// linkImportProject creates a project directory <tmp>/repo/.scion with the
// given settings and returns the .scion directory, its settings file and
// the link path of the project.
func linkImportProject(t *testing.T, settings string) (projectDir, settingsPath, linkPath string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	projectDir = filepath.Join(repo, DotScion)
	settingsPath = filepath.Join(projectDir, "settings.yaml")
	writeCLIContextFile(t, settingsPath, settings)
	return projectDir, settingsPath, LinkPathForProject(projectDir)
}

// countProjectSettingsReads counts calls to projectHubLinkSettings.
func countProjectSettingsReads(t *testing.T) *int {
	t.Helper()
	orig := projectHubLinkSettings
	n := new(int)
	projectHubLinkSettings = func(dir string) projectHubLink {
		*n++
		return orig(dir)
	}
	t.Cleanup(func() { projectHubLinkSettings = orig })
	return n
}

// A linked project's own hub.endpoint becomes a hub entry plus a link
// entry for the project's hub ID, with its transport; the import is
// recorded, the caller's context is updated and no settings file changes.
func TestLinkEntryForProject_ImportsTheProjectEndpoint(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	settings := "hub:\n  endpoint: https://Proj.Example.com/\n  projectId: hub-1\n  linked: true\n  transport:\n    mode: iap\n    audience: aud-1\n"
	projectDir, settingsPath, linkPath := linkImportProject(t, settings)

	c := newCLIContext()
	p, h, err := LinkEntryForProject(c, projectDir, importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || h == nil {
		t.Fatal("no link entry")
	}
	if p.Hub != "proj" || p.ID != "hub-1" || p.LocalPath != linkPath || p.Source != KnownProjectSourceLink {
		t.Errorf("link entry = %+v", *p)
	}
	if h.Endpoint != "https://proj.example.com" {
		t.Errorf("hub endpoint = %q", h.Endpoint)
	}
	if want := (&HubTransportConfig{Mode: "iap", Audience: "aud-1"}); !reflect.DeepEqual(h.Transport, want) {
		t.Errorf("transport = %+v, want %+v", h.Transport, want)
	}
	if c.Hubs["proj"] == nil || !c.ProjectImported(linkPath) {
		t.Errorf("caller's context not updated: %+v", c)
	}
	if c.CurrentHub != "" {
		t.Errorf("current hub = %q: the project import never selects a hub", c.CurrentHub)
	}

	onDisk, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	if lp, lh := onDisk.LinkEntryFor(linkPath); lp == nil || lh == nil || lp.ID != "hub-1" || !onDisk.ProjectImported(linkPath) {
		t.Errorf("context.yaml = %+v", onDisk)
	}
	if got := readFileString(t, settingsPath); got != settings {
		t.Errorf("the project settings changed:\n%s", got)
	}
	if _, err := os.Stat(ctxPath); err != nil {
		t.Errorf("context.yaml: %v", err)
	}
}

// A linked project without its own endpoint is linked to the global
// settings' hub.endpoint, with the global transport.
func TestLinkEntryForProject_InheritsTheGlobalEndpoint(t *testing.T) {
	cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://glob.example.com\n  transport:\n    mode: cloudrun_invoker\n    audience: aud-g\n")
	projectDir, _, _ := linkImportProject(t, "hub:\n  projectId: hub-2\n  linked: true\n")

	p, h, err := LinkEntryForProject(newCLIContext(), projectDir, importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || h == nil || p.ID != "hub-2" || h.Endpoint != "https://glob.example.com" {
		t.Fatalf("link entry = %+v, hub = %+v", p, h)
	}
	if h.Transport == nil || h.Transport.Audience != "aud-g" {
		t.Errorf("transport = %+v, want the global one", h.Transport)
	}
}

// With hub.linked unset, a hub project ID that differs from the local
// project ID still marks a link (made before hub.linked was recorded).
func TestLinkEntryForProject_LegacyLinkWithoutLinkedFlag(t *testing.T) {
	cliContextTestHome(t)
	projectDir, _, _ := linkImportProject(t, "project_id: local-1\nhub:\n  endpoint: https://proj.example.com\n  projectId: hub-3\n")

	p, h, err := LinkEntryForProject(newCLIContext(), projectDir, importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || h == nil || p.ID != "hub-3" {
		t.Fatalf("link entry = %+v, want hub-3", p)
	}
}

// A project that is not linked (hub.linked=false, or never linked) is not
// imported, recorded or written: its endpoint is ignored.
func TestLinkEntryForProject_NotLinked(t *testing.T) {
	for name, settings := range map[string]string{
		"unlinked":     "hub:\n  endpoint: https://proj.example.com\n  projectId: hub-1\n  linked: false\n",
		"never linked": "project_id: same\nhub:\n  endpoint: https://proj.example.com\n  projectId: same\n",
		"no hub ids":   "hub:\n  endpoint: https://proj.example.com\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctxPath := cliContextTestHome(t)
			projectDir, _, linkPath := linkImportProject(t, settings)
			c := newCLIContext()
			p, h, err := LinkEntryForProject(c, projectDir, importNamer)
			if err != nil || p != nil || h != nil {
				t.Fatalf("= %+v, %+v, %v; want nothing", p, h, err)
			}
			if len(c.Hubs) != 0 || c.ProjectImported(linkPath) {
				t.Errorf("context = %+v, want unchanged", c)
			}
			if _, err := os.Stat(ctxPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("context.yaml was written: %v", err)
			}
		})
	}
}

// The import runs once per project: an existing link entry is returned
// without reading the settings, a changed endpoint is not followed, and a
// link entry removed later is not brought back.
func TestLinkEntryForProject_Once(t *testing.T) {
	cliContextTestHome(t)
	projectDir, settingsPath, linkPath := linkImportProject(t, "hub:\n  endpoint: https://proj.example.com\n  projectId: hub-1\n  linked: true\n")
	reads := countProjectSettingsReads(t)

	c := newCLIContext()
	if _, _, err := LinkEntryForProject(c, projectDir, importNamer); err != nil {
		t.Fatal(err)
	}
	if *reads != 1 {
		t.Fatalf("settings read %d times, want 1", *reads)
	}

	writeCLIContextFile(t, settingsPath, "hub:\n  endpoint: https://other.example.com\n  projectId: hub-1\n  linked: true\n")
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	_, h, err := LinkEntryForProject(c, projectDir, importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if h == nil || h.Endpoint != "https://proj.example.com" {
		t.Errorf("hub = %+v, want the imported endpoint, not the changed one", h)
	}

	if err := UpdateCLIContext(func(c *CLIContext) error {
		c.RemoveLinkEntry(linkPath)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err = LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	if p, h, err := LinkEntryForProject(c, projectDir, importNamer); err != nil || p != nil || h != nil {
		t.Errorf("after removing the link entry = %+v, %+v, %v; want nothing", p, h, err)
	}
	if *reads != 1 {
		t.Errorf("settings read %d times, want 1", *reads)
	}
}

// An entry that already has the endpoint is reused and keeps its own
// transport.
func TestLinkEntryForProject_ReusesAnEntry(t *testing.T) {
	cliContextTestHome(t)
	projectDir, _, _ := linkImportProject(t, "hub:\n  endpoint: https://hub.example.com/\n  projectId: hub-1\n  linked: true\n  transport:\n    mode: iap\n")
	c := newCLIContext()
	if err := c.AddHub("prod", "https://hub.example.com"); err != nil {
		t.Fatal(err)
	}
	c.Hubs["prod"].Transport = &HubTransportConfig{Mode: "cloudrun_invoker"}
	if err := UpdateCLIContext(func(fresh *CLIContext) error { *fresh = *c; return nil }); err != nil {
		t.Fatal(err)
	}

	p, h, err := LinkEntryForProject(c, projectDir, importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Hub != "prod" || len(c.Hubs) != 1 || h.Transport.Mode != "cloudrun_invoker" {
		t.Errorf("link = %+v, hubs = %+v", p, c.Hubs)
	}
}

// A linked project with no usable endpoint (one carrying user info, and no
// global endpoint) gets no link entry, but the import is recorded so its
// settings are not read again.
func TestLinkEntryForProject_InvalidEndpointRecorded(t *testing.T) {
	cliContextTestHome(t)
	projectDir, _, linkPath := linkImportProject(t, "hub:\n  endpoint: https://user:pw@proj.example.com\n  projectId: hub-1\n  linked: true\n")
	c := newCLIContext()
	p, h, err := LinkEntryForProject(c, projectDir, importNamer)
	if err != nil || p != nil || h != nil {
		t.Fatalf("= %+v, %+v, %v; want nothing", p, h, err)
	}
	if len(c.Hubs) != 0 || !c.ProjectImported(linkPath) {
		t.Errorf("context = %+v, want no hub and the import recorded", c)
	}
}

// A context file written by a newer version, and the global directory,
// are never imported into.
func TestLinkEntryForProject_Skipped(t *testing.T) {
	t.Run("newer version", func(t *testing.T) {
		ctxPath := cliContextTestHome(t)
		writeCLIContextFile(t, ctxPath, "version: 99\n")
		projectDir, _, _ := linkImportProject(t, "hub:\n  endpoint: https://proj.example.com\n  projectId: hub-1\n  linked: true\n")
		c, err := LoadCLIContext()
		if err != nil {
			t.Fatal(err)
		}
		if p, _, err := LinkEntryForProject(c, projectDir, importNamer); err != nil || p != nil {
			t.Fatalf("= %+v, %v; want nothing", p, err)
		}
		if got := readFileString(t, ctxPath); got != "version: 99\n" {
			t.Errorf("context.yaml changed:\n%s", got)
		}
	})
	t.Run("global directory", func(t *testing.T) {
		ctxPath := cliContextTestHome(t)
		writeGlobalSettingsFile(t, "hub:\n  endpoint: https://glob.example.com\n  projectId: hub-1\n  linked: true\n")
		globalDir := filepath.Join(os.Getenv("HOME"), GlobalDir)
		if p, _, err := LinkEntryForProject(newCLIContext(), globalDir, importNamer); err != nil || p != nil {
			t.Fatalf("= %+v, %v; want nothing", p, err)
		}
		if _, err := os.Stat(ctxPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("context.yaml was written: %v", err)
		}
	})
}

// When the import cannot be saved, the caller's context holds it anyway,
// with an error wrapping ErrCLIContextImport.
func TestLinkEntryForProject_SaveFailureKeepsView(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	cliContextTestHome(t)
	projectDir, _, _ := linkImportProject(t, "hub:\n  endpoint: https://proj.example.com\n  projectId: hub-1\n  linked: true\n")
	dir := filepath.Join(os.Getenv("HOME"), GlobalDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c := newCLIContext()
	p, h, err := LinkEntryForProject(c, projectDir, importNamer)
	if !errors.Is(err, ErrCLIContextImport) {
		t.Fatalf("err = %v, want ErrCLIContextImport", err)
	}
	if p == nil || h == nil || h.Endpoint != "https://proj.example.com" {
		t.Errorf("= %+v, %+v; want the imported link", p, h)
	}
}

// A link entry removed with 'scion hub remove' (RemoveHub) or 'scion hub
// unlink' (RemoveLinkEntry), also one the import did not write, is not
// brought back by the import: the removal marks the path as imported, so
// the project's settings are not read again and no hub entry is re-created.
func TestLinkEntryForProject_NotBroughtBackAfterRemoval(t *testing.T) {
	for name, remove := range map[string]func(c *CLIContext, linkPath string) error{
		"hub remove": func(c *CLIContext, _ string) error {
			_, _, err := c.RemoveHub("prod")
			return err
		},
		"hub unlink": func(c *CLIContext, linkPath string) error {
			if !c.RemoveLinkEntry(linkPath) {
				return errors.New("no link entry removed")
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			cliContextTestHome(t)
			writeGlobalSettingsFile(t, "hub:\n  endpoint: https://glob.example.com\n")
			// Linked as 'scion hub link' links today: no endpoint in settings.
			projectDir, _, linkPath := linkImportProject(t, "hub:\n  projectId: hub-1\n  linked: true\n")
			if err := UpdateCLIContext(func(c *CLIContext) error {
				if err := c.AddHub("prod", "https://prod.example.com"); err != nil {
					return err
				}
				return c.SetLinkEntry("prod", KnownProject{ID: "hub-1"}, linkPath, time.Now())
			}); err != nil {
				t.Fatal(err)
			}
			if err := UpdateCLIContext(func(c *CLIContext) error { return remove(c, linkPath) }); err != nil {
				t.Fatal(err)
			}
			reads := countProjectSettingsReads(t)

			c, err := LoadCLIContext()
			if err != nil {
				t.Fatal(err)
			}
			p, h, err := LinkEntryForProject(c, projectDir, importNamer)
			if err != nil || p != nil || h != nil {
				t.Fatalf("= %+v, %+v, %v; want nothing: the removed link came back", p, h, err)
			}
			if _, gh := c.HubByEndpoint("https://glob.example.com"); gh != nil {
				t.Errorf("hubs = %v: the global hub entry was re-created", c.HubNames())
			}
			if *reads != 0 {
				t.Errorf("settings read %d times, want 0", *reads)
			}
		})
	}
}

// The new import markers survive a save and load.
func TestCLIContextImportMarkersRoundTrip(t *testing.T) {
	cliContextTestHome(t)
	now := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	if err := UpdateCLIContext(func(c *CLIContext) error {
		c.ImportCurrentHub("https://hub.example.com", nil, "hub", now)
		c.markProjectImported("/src/foo", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Imported.Current.Equal(now) || !c.Imported.Projects["/src/foo"].Equal(now) {
		t.Errorf("imported = %+v", c.Imported)
	}
}

// A versioned project file keeps its ID at hub.project_id: a linked one
// (hub.linked=true) is imported with that ID, and one without hub.linked
// is not (its ID is its own local ID).
func TestLinkEntryForProject_VersionedFile(t *testing.T) {
	t.Run("linked", func(t *testing.T) {
		cliContextTestHome(t)
		projectDir, _, _ := linkImportProject(t, "schema_version: \"1\"\nhub:\n  linked: true\n  endpoint: https://proj.example.com\n  project_id: hub-v1\n  transport:\n    mode: iap\n    audience: aud-v1\n")
		p, h, err := LinkEntryForProject(newCLIContext(), projectDir, importNamer)
		if err != nil {
			t.Fatal(err)
		}
		if p == nil || h == nil || p.ID != "hub-v1" || h.Endpoint != "https://proj.example.com" {
			t.Fatalf("link entry = %+v, hub = %+v", p, h)
		}
		if h.Transport == nil || h.Transport.Audience != "aud-v1" {
			t.Errorf("transport = %+v, want the file's", h.Transport)
		}
	})
	t.Run("linked unset", func(t *testing.T) {
		cliContextTestHome(t)
		projectDir, _, _ := linkImportProject(t, "schema_version: \"1\"\nhub:\n  endpoint: https://proj.example.com\n  project_id: local-v1\n")
		p, _, err := LinkEntryForProject(newCLIContext(), projectDir, importNamer)
		if err != nil || p != nil {
			t.Fatalf("= %+v, %v; want nothing", p, err)
		}
	})
}

// A directory left by the removed 'scion hub shadow' (project type shadow
// in its external settings) is imported as a link entry to the shadow's
// hub and hub project.
func TestLinkEntryForProject_ShadowDirectory(t *testing.T) {
	cliContextTestHome(t)
	projectDir, _, _ := linkImportProject(t, "schema_version: \"1\"\nproject_type: shadow\nhub:\n  enabled: true\n  endpoint: https://shadow.example.com\n  project_id: shadow-project\n")
	p, h, err := LinkEntryForProject(newCLIContext(), projectDir, importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || h == nil || p.ID != "shadow-project" || h.Endpoint != "https://shadow.example.com" {
		t.Fatalf("link entry = %+v, hub = %+v", p, h)
	}
}

// assertNoCLIContextFiles fails when context.yaml or its lock exists.
func assertNoCLIContextFiles(t *testing.T, ctxPath string) {
	t.Helper()
	for _, p := range []string{ctxPath, ctxPath + ".lock"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists (stat error %v)", p, err)
		}
	}
}

// The in-memory global import gives the same context as the saved one,
// and writes nothing (ptone/scion#5483).
func TestLoadCLIContextImportedInMemory_WritesNothing(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://Hub.Example.com/\n")

	c, err := LoadCLIContextImportedInMemory(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentHub != "hub" || c.Hubs["hub"] == nil || c.Hubs["hub"].Endpoint != "https://hub.example.com" {
		t.Fatalf("in-memory import = %+v, current %q", c.Hubs, c.CurrentHub)
	}
	assertNoCLIContextFiles(t, ctxPath)

	// The saving import afterwards gives the same hub.
	saved, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CurrentHub != c.CurrentHub || saved.Hubs["hub"].Endpoint != c.Hubs["hub"].Endpoint {
		t.Errorf("saved import %+v differs from the in-memory one %+v", saved.Hubs, c.Hubs)
	}
}

// The in-memory project link import gives the link entry and writes
// nothing (ptone/scion#5483).
func TestLinkEntryForProjectInMemory_WritesNothing(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	settings := "hub:\n  endpoint: https://Proj.Example.com/\n  projectId: hub-1\n  linked: true\n"
	projectDir, settingsPath, linkPath := linkImportProject(t, settings)

	c := newCLIContext()
	p, h := LinkEntryForProjectInMemory(c, projectDir, importNamer)
	if p == nil || h == nil || p.ID != "hub-1" || p.LocalPath != linkPath || h.Endpoint != "https://proj.example.com" {
		t.Fatalf("link entry = %+v, hub %+v", p, h)
	}
	if !c.ProjectImported(linkPath) {
		t.Errorf("caller's context lacks the import: %+v", c)
	}
	assertNoCLIContextFiles(t, ctxPath)
	if got := readFileString(t, settingsPath); got != settings {
		t.Errorf("the project settings changed:\n%s", got)
	}
}
