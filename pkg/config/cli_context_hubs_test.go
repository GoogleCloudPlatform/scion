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
	"strings"
	"testing"
	"time"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://hub.example.com":       "https://hub.example.com",
		"https://hub.example.com/":      "https://hub.example.com",
		"  https://hub.example.com//  ": "https://hub.example.com",
		"HTTPS://Hub.Example.COM":       "https://hub.example.com",
		"https://hub.example.com:443":   "https://hub.example.com",
		"http://hub.example.com:80/":    "http://hub.example.com",
		"http://hub.example.com:443":    "http://hub.example.com:443",
		"https://hub.example.com:8443":  "https://hub.example.com:8443",
		"http://127.0.0.1:8080":         "http://127.0.0.1:8080",
		"https://hub.example.com/Base/": "https://hub.example.com/Base",
		"http://[::1]:8080/":            "http://[::1]:8080",
		"http://[::1]:80":               "http://[::1]",
		"not a url/":                    "not a url",
		"":                              "",
	}
	for in, want := range tests {
		if got := NormalizeEndpoint(in); got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCLIContextAddHub_NameValidation(t *testing.T) {
	valid := []string{"prod", "a", "0", "my-hub-2", strings.Repeat("a", 63)}
	invalid := []string{"", "Prod", "-prod", "prod_1", "prod.example", "a/b", "a:b", "https://x", strings.Repeat("a", 64)}
	for _, name := range valid {
		c := newCLIContext()
		if err := c.AddHub(name, "https://hub.example.com"); err != nil {
			t.Errorf("AddHub(%q): %v, want nil", name, err)
		}
	}
	for _, name := range invalid {
		c := newCLIContext()
		err := c.AddHub(name, "https://hub.example.com")
		if err == nil || !strings.Contains(err.Error(), "invalid hub name") {
			t.Errorf("AddHub(%q) = %v, want an invalid name error", name, err)
		}
	}
}

func TestCLIContextAddHub_URLValidation(t *testing.T) {
	for _, u := range []string{
		"", "hub.example.com", "ftp://hub.example.com", "https://", "prod",
		"https://user:pw@hub.example.com", "https://user@hub.example.com",
		"https://hub.example.com/?token=x", "https://hub.example.com?", "https://hub.example.com/#frag", "https://hub.example.com#",
	} {
		c := newCLIContext()
		err := c.AddHub("prod", u)
		if err == nil || !strings.Contains(err.Error(), "invalid hub URL") {
			t.Errorf("AddHub(prod, %q) = %v, want an invalid URL error", u, err)
		}
	}
}

func TestCLIContextAddHub_Duplicates(t *testing.T) {
	c := newCLIContext()
	if err := c.AddHub("prod", "https://Hub.Example.com/"); err != nil {
		t.Fatal(err)
	}
	if got := c.Hubs["prod"].Endpoint; got != "https://hub.example.com" {
		t.Errorf("stored endpoint = %q, want the normalised URL", got)
	}

	err := c.AddHub("prod", "https://other.example.com")
	if err == nil || !strings.Contains(err.Error(), `a hub named "prod" already exists`) {
		t.Errorf("duplicate name: err = %v", err)
	}

	// One entry per endpoint: another spelling of the same URL is refused,
	// and the error names the existing entry.
	err = c.AddHub("production", "https://hub.example.com:443")
	if err == nil || !strings.Contains(err.Error(), `already registered as hub "prod"`) {
		t.Errorf("duplicate URL: err = %v", err)
	}
	if len(c.Hubs) != 1 {
		t.Errorf("hubs = %v, want only prod", c.HubNames())
	}
}

func TestCLIContextRenameHub_RewritesReferences(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	c := newCLIContext()
	_ = c.AddHub("prod", "https://hub.example.com")
	_ = c.AddHub("staging", "https://staging.example.com")
	if err := c.SelectProject("prod", KnownProject{ID: "p1", Slug: "foo"}, now); err != nil {
		t.Fatal(err)
	}
	c.Projects = append(c.Projects, KnownProject{Hub: "staging", ID: "s1"})
	c.Refreshed = map[string]time.Time{"prod": now, "staging": now}

	if err := c.RenameHub("prod", "production"); err != nil {
		t.Fatal(err)
	}
	if c.CurrentHub != "production" {
		t.Errorf("current hub = %q, want production", c.CurrentHub)
	}
	if c.Hubs["prod"] != nil || c.Hubs["production"] == nil || c.Hubs["production"].CurrentProject.ID != "p1" {
		t.Errorf("hubs after rename = %+v", c.Hubs)
	}
	if c.Projects[0].Hub != "production" || c.Projects[1].Hub != "staging" {
		t.Errorf("projects after rename = %+v", c.Projects)
	}
	if _, ok := c.Refreshed["prod"]; ok || !c.Refreshed["production"].Equal(now) || !c.Refreshed["staging"].Equal(now) {
		t.Errorf("refreshed after rename = %v", c.Refreshed)
	}

	for _, tc := range []struct{ old, new, want string }{
		{"missing", "x", `no hub named "missing"`},
		{"production", "staging", `a hub named "staging" already exists`},
		{"production", "Bad_Name", "invalid hub name"},
	} {
		if err := c.RenameHub(tc.old, tc.new); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("RenameHub(%q, %q) = %v, want %q", tc.old, tc.new, err, tc.want)
		}
	}
}

func TestCLIContextRemoveHub(t *testing.T) {
	now := time.Now()
	c := newCLIContext()
	_ = c.AddHub("prod", "https://hub.example.com")
	_ = c.AddHub("staging", "https://staging.example.com")
	_ = c.SelectProject("prod", KnownProject{ID: "p1"}, now)
	c.Projects = append(c.Projects, KnownProject{Hub: "staging", ID: "s1"})
	c.Refreshed = map[string]time.Time{"prod": now}

	// Removing a hub that is not current keeps the current hub.
	removed, wasCurrent, err := c.RemoveHub("staging")
	if err != nil || wasCurrent || removed.Endpoint != "https://staging.example.com" {
		t.Fatalf("RemoveHub(staging) = %+v, %v, %v", removed, wasCurrent, err)
	}
	if c.CurrentHub != "prod" || len(c.Projects) != 1 || c.Projects[0].Hub != "prod" {
		t.Errorf("after removing staging: current %q, projects %+v", c.CurrentHub, c.Projects)
	}

	// Removing the current hub clears it, with its projects and refresh time.
	_, wasCurrent, err = c.RemoveHub("prod")
	if err != nil || !wasCurrent {
		t.Fatalf("RemoveHub(prod) = %v, %v", wasCurrent, err)
	}
	if c.CurrentHub != "" || len(c.Hubs) != 0 || c.Projects != nil || len(c.Refreshed) != 0 {
		t.Errorf("after removing prod: %+v", c)
	}

	if _, _, err := c.RemoveHub("prod"); err == nil {
		t.Error("removing a missing hub must fail")
	}
}

func TestCLIContextLookupHub(t *testing.T) {
	c := newCLIContext()
	_ = c.AddHub("prod", "https://hub.example.com")
	for in, want := range map[string]string{
		"prod":                      "prod",
		"https://HUB.example.com/":  "prod",
		"staging":                   "",
		"https://other.example.com": "",
	} {
		if got, _ := c.LookupHub(in); got != want {
			t.Errorf("LookupHub(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeGlobalSettingsFile writes ~/.scion/settings.yaml under the test HOME
// and returns its path.
func writeGlobalSettingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), GlobalDir, "settings.yaml")
	writeCLIContextFile(t, path, content)
	return path
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// importNamer names the local workstation hub "local", as cmd does, using a
// fixed host as the stand-in for a local hub.
func importNamer(endpoint string) string {
	if strings.Contains(endpoint, "127.0.0.1") {
		return "local"
	}
	return DeriveHubName(endpoint)
}

// The global hub.endpoint becomes a hub entry, but not the current hub;
// the transport is copied onto it, the import is written once and
// recorded, and the settings file is not changed.
func TestLoadCLIContextImported_ImportsGlobalEndpoint(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	settings := "hub:\n  endpoint: https://Hub.Example.com/\n  transport:\n    mode: iap\n    audience: aud-1\n"
	settingsPath := writeGlobalSettingsFile(t, settings)

	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hubs["hub"] == nil || c.Hubs["hub"].Endpoint != "https://hub.example.com" {
		t.Fatalf("imported context = %+v", c)
	}
	if c.CurrentHub != "" {
		t.Errorf("current hub = %q, want none: the import never selects a hub", c.CurrentHub)
	}
	want := &HubTransportConfig{Mode: "iap", Audience: "aud-1"}
	if !reflect.DeepEqual(c.Hubs["hub"].Transport, want) {
		t.Errorf("transport = %+v, want %+v", c.Hubs["hub"].Transport, want)
	}
	if !c.GlobalImported() {
		t.Error("imported.global is not recorded")
	}

	// Written, under the lock, with the import recorded.
	onDisk, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.CurrentHub != "" || !onDisk.GlobalImported() || onDisk.Hubs["hub"] == nil || onDisk.Hubs["hub"].Transport == nil {
		t.Fatalf("context.yaml = %+v", onDisk)
	}
	if got := readFileString(t, settingsPath); got != settings {
		t.Errorf("the settings file changed:\n%s", got)
	}

	// Once: a later change to the global endpoint is not imported again,
	// and the file is not rewritten. With no current hub, the CLI keeps
	// following the global hub.endpoint itself (tested in cmd).
	before := readFileString(t, ctxPath)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://other.example.com\n")
	c, err = LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hubs) != 1 || c.CurrentHub != "" {
		t.Errorf("second load re-imported or selected: %+v, current %q", c.Hubs, c.CurrentHub)
	}
	if got := readFileString(t, ctxPath); got != before {
		t.Errorf("second load rewrote context.yaml:\n%s", got)
	}
}

func TestLoadCLIContextImported_LocalName(t *testing.T) {
	cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: http://127.0.0.1:8080\n")
	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentHub != "" || c.Hubs["local"] == nil || c.Hubs["local"].Endpoint != "http://127.0.0.1:8080" || c.Hubs["local"].Transport != nil {
		t.Errorf("imported context = %+v", c.Hubs)
	}
}

// No global endpoint: nothing is imported and no file is created.
func TestLoadCLIContextImported_NoEndpointWritesNothing(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	writeGlobalSettingsFile(t, "image_registry: example\n")
	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hubs) != 0 || c.GlobalImported() {
		t.Errorf("context = %+v, want empty", c)
	}
	if _, err := os.Stat(ctxPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("context.yaml was created: %v", err)
	}
}

// Environment variables are not imported: only the settings file counts.
func TestLoadCLIContextImported_IgnoresEnv(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")
	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hubs) != 0 {
		t.Errorf("hubs = %v, want none", c.HubNames())
	}
	if _, err := os.Stat(ctxPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("context.yaml was created: %v", err)
	}
}

// An existing current hub stays current; a name collision is de-duplicated;
// an entry that already has the endpoint is reused, keeps its transport and
// is not selected.
func TestLoadCLIContextImported_ExistingEntries(t *testing.T) {
	t.Run("collision", func(t *testing.T) {
		cliContextTestHome(t)
		writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.other.com\n")
		if err := UpdateCLIContext(func(c *CLIContext) error {
			if err := c.AddHub("hub", "https://hub.example.com"); err != nil {
				return err
			}
			c.CurrentHub = "hub"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		c, err := LoadCLIContextImported(importNamer)
		if err != nil {
			t.Fatal(err)
		}
		if c.CurrentHub != "hub" || c.Hubs["hub-2"] == nil || c.Hubs["hub-2"].Endpoint != "https://hub.other.com" {
			t.Errorf("hubs = %+v, current %q", c.Hubs, c.CurrentHub)
		}
	})
	t.Run("same endpoint", func(t *testing.T) {
		cliContextTestHome(t)
		writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com/\n  transport:\n    mode: iap\n")
		if err := UpdateCLIContext(func(c *CLIContext) error {
			if err := c.AddHub("prod", "https://hub.example.com"); err != nil {
				return err
			}
			c.Hubs["prod"].Transport = &HubTransportConfig{Mode: "cloudrun_invoker"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		c, err := LoadCLIContextImported(importNamer)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Hubs) != 1 || c.CurrentHub != "" || c.Hubs["prod"].Transport.Mode != "cloudrun_invoker" {
			t.Errorf("hubs = %+v, current %q", c.Hubs, c.CurrentHub)
		}
	})
}

// A file written by a newer version is read-only: no import, no write.
func TestLoadCLIContextImported_NewerVersionNotImported(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	content := "version: 99\n"
	writeCLIContextFile(t, ctxPath, content)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com\n")
	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hubs) != 0 {
		t.Errorf("hubs = %v, want none", c.HubNames())
	}
	if got := readFileString(t, ctxPath); got != content {
		t.Errorf("context.yaml changed:\n%s", got)
	}
}

// A corrupt file returns the corrupt error and is not imported over.
func TestLoadCLIContextImported_CorruptFileNotImported(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	writeCLIContextFile(t, ctxPath, "{not yaml")
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com\n")
	_, err := LoadCLIContextImported(importNamer)
	if !errors.Is(err, ErrCLIContextCorrupt) {
		t.Fatalf("err = %v, want ErrCLIContextCorrupt", err)
	}
	if got := readFileString(t, ctxPath); got != "{not yaml" {
		t.Errorf("context.yaml changed:\n%s", got)
	}
}

// When the import cannot be saved, the context still holds it, with an
// error wrapping ErrCLIContextImport.
func TestLoadCLIContextImported_SaveFailureKeepsView(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com\n")
	dir := filepath.Join(os.Getenv("HOME"), GlobalDir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c, err := LoadCLIContextImported(importNamer)
	if !errors.Is(err, ErrCLIContextImport) {
		t.Fatalf("err = %v, want ErrCLIContextImport", err)
	}
	if c.Hubs["hub"] == nil || c.CurrentHub != "" {
		t.Errorf("context = %+v, want the imported view", c)
	}
}

// A global endpoint that is not a valid hub URL, such as one carrying user
// info, is not imported: nothing is written and the import stays pending.
func TestLoadCLIContextImported_InvalidEndpointNotImported(t *testing.T) {
	ctxPath := cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://user:pw@hub.example.com\n")
	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hubs) != 0 || c.GlobalImported() {
		t.Errorf("context = %+v, want nothing imported", c)
	}
	if _, err := os.Stat(ctxPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("context.yaml was created: %v", err)
	}
}
