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

package agentplugin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadManifestJSON(t *testing.T, content string) (*Manifest, []Diagnostic, error) {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"plugin.json": content})
	return LoadManifest(openRoot(t, dir))
}

func TestLoadManifestMinimal(t *testing.T) {
	m, diags, err := loadManifestJSON(t, minimalManifest)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "minimal-plugin" || m.Schema != PluginSchemaV1 || len(diags) != 0 {
		t.Fatalf("got %+v diags %v", m, diags)
	}
}

func TestLoadManifestFull(t *testing.T) {
	m, diags, err := loadManifestJSON(t, `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "plugin-name", "version": "1.2.0", "description": "Brief",
  "author": {"name": "Author Name", "email": "a@example.com", "url": "https://example.com"},
  "homepage": "https://docs.example.com", "repository": "https://github.com/example/plugin",
  "license": "MIT", "keywords": ["k1", "k2"],
  "extensions": {"com.example.client": {"setting": true}}
}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 0 {
		t.Fatalf("diags = %v", diags)
	}
	if m.Version != "1.2.0" || m.Description != "Brief" || m.License != "MIT" || m.Homepage == "" || m.Repository == "" {
		t.Fatalf("metadata = %+v", m)
	}
	if m.Author == nil || m.Author.Email != "a@example.com" || len(m.Keywords) != 2 || !strings.Contains(string(m.Extensions), "com.example.client") {
		t.Fatalf("author/keywords/extensions = %+v", m)
	}
}

func TestLoadManifestWarnings(t *testing.T) {
	m, diags, err := loadManifestJSON(t, `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "p", "foo": 1, "extensions": "nope"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m.Extensions != nil {
		t.Errorf("non-object extensions kept: %s", m.Extensions)
	}
	if len(diags) != 2 || !strings.Contains(diags[0].Message, "extensions") || diags[1].Message != `unknown manifest field "foo" ignored` {
		t.Fatalf("diags = %v", diags)
	}
	for _, d := range diags {
		if d.Severity != SeverityWarning || d.Plugin != "" {
			t.Errorf("diag = %+v", d)
		}
	}
}

func TestLoadManifestFatal(t *testing.T) {
	const s = `"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"`
	cases := map[string]string{
		"author extra field":   `{` + s + `, "name": "p", "author": {"name": "x", "twitter": "y"}}`,
		"author not object":    `{` + s + `, "name": "p", "author": "x"}`,
		"keywords not strings": `{` + s + `, "name": "p", "keywords": ["a", 1]}`,
		"keywords not array":   `{` + s + `, "name": "p", "keywords": "a"}`,
		"missing name":         `{` + s + `}`,
		"bad name":             `{` + s + `, "name": "Bad"}`,
		"name not string":      `{` + s + `, "name": 3}`,
		"version not string":   `{` + s + `, "name": "p", "version": 1}`,
		"version null":         `{` + s + `, "name": "p", "version": null}`,
		"missing schema":       `{"name": "p"}`,
		"not object":           `["x"]`,
		"bad json":             `{`,
		"trailing data":        `{` + s + `, "name": "p"} {}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := loadManifestJSON(t, content); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestLoadManifestUnsupportedSchema(t *testing.T) {
	_, _, err := loadManifestJSON(t, `{"$schema": "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json", "name": "acme"}`)
	var se *UnsupportedSchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want UnsupportedSchemaError", err)
	}
	want := `plugin "acme": unsupported Agent Plugins version "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json"; Scion supports 1.0.0`
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
}

func TestLoadManifestSymlinkAndSize(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"real.json": minimalManifest})
	if err := os.Symlink("real.json", filepath.Join(dir, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadManifest(openRoot(t, dir)); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink manifest: err = %v", err)
	}

	big := t.TempDir()
	content := `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "p", "description": "` +
		strings.Repeat("x", 1<<20) + `"}`
	writeFiles(t, big, map[string]string{"plugin.json": content})
	if _, _, err := LoadManifest(openRoot(t, big)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("big manifest: err = %v", err)
	}

	missing := t.TempDir()
	if _, _, err := LoadManifest(openRoot(t, missing)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing manifest: err = %v", err)
	}
}
