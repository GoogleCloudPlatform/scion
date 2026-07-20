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

package resources

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

func TestBuiltinTemplates(t *testing.T) {
	templates := BuiltinTemplates()
	if len(templates) < 1 {
		t.Fatalf("expected at least 1 template, got %d", len(templates))
	}

	found := false
	for _, tmpl := range templates {
		if tmpl.Name == "default" {
			found = true
			if tmpl.Kind != storage.ResourceKindTemplate {
				t.Errorf("default template kind = %q, want %q", tmpl.Kind, storage.ResourceKindTemplate)
			}
			if tmpl.Scope != "global" {
				t.Errorf("default template scope = %q, want %q", tmpl.Scope, "global")
			}
			if tmpl.ScopeID != "" {
				t.Errorf("default template scopeID = %q, want empty", tmpl.ScopeID)
			}
			assertSourceURL(t, tmpl)
			assertFileReadable(t, tmpl, "scion-agent.yaml")
		}
	}
	if !found {
		t.Error("default template not found in BuiltinTemplates()")
	}
}

func TestBuiltinHarnessConfigs(t *testing.T) {
	configs := BuiltinHarnessConfigs()

	if len(configs) == 0 {
		t.Fatal("expected at least 1 harness-config, got 0")
	}

	seen := make(map[string]bool)
	for _, cfg := range configs {
		if cfg.Kind != storage.ResourceKindHarnessConfig {
			t.Errorf("harness-config %q kind = %q, want %q", cfg.Name, cfg.Kind, storage.ResourceKindHarnessConfig)
		}
		if cfg.Scope != "global" {
			t.Errorf("harness-config %q scope = %q, want %q", cfg.Name, cfg.Scope, "global")
		}
		if cfg.ScopeID != "" {
			t.Errorf("harness-config %q scopeID = %q, want empty", cfg.Name, cfg.ScopeID)
		}
		assertSourceURL(t, cfg)
		if seen[cfg.Name] {
			t.Errorf("duplicate harness-config %q", cfg.Name)
		}
		seen[cfg.Name] = true
	}

	if !seen["claude"] {
		t.Error("expected harness-config \"claude\" not found")
	}
}

func TestBuiltinHarnessConfigsExcludesGemini(t *testing.T) {
	for _, cfg := range BuiltinHarnessConfigs() {
		if cfg.Name == "gemini" {
			t.Error("gemini should not be in BuiltinHarnessConfigs()")
		}
	}
}

func TestBuiltinHarnessConfigsReadConfigYAML(t *testing.T) {
	for _, cfg := range BuiltinHarnessConfigs() {
		assertFileReadable(t, cfg, "config.yaml")
	}
}

func TestBuiltinResources(t *testing.T) {
	all := BuiltinResources()
	templates := BuiltinTemplates()
	configs := BuiltinHarnessConfigs()

	want := len(templates) + len(configs)
	if len(all) != want {
		t.Errorf("BuiltinResources() returned %d entries, want %d (templates=%d + configs=%d)",
			len(all), want, len(templates), len(configs))
	}
}

func TestSourceURLFormat(t *testing.T) {
	const harnessPrefix = "git+https://github.com/GoogleCloudPlatform/scion/harnesses/"
	const builtinPrefix = "builtin://scion/"

	for _, r := range BuiltinResources() {
		if r.Kind == storage.ResourceKindHarnessConfig {
			if !strings.HasPrefix(r.SourceURL, harnessPrefix) {
				t.Errorf("%s %q SourceURL = %q, want prefix %q", r.Kind, r.Name, r.SourceURL, harnessPrefix)
				continue
			}
			name := strings.TrimPrefix(r.SourceURL, harnessPrefix)
			if name != r.Name {
				t.Errorf("%s %q SourceURL name = %q, want %q", r.Kind, r.Name, name, r.Name)
			}
		} else {
			if !strings.HasPrefix(r.SourceURL, builtinPrefix) {
				t.Errorf("%s %q SourceURL = %q, want prefix %q", r.Kind, r.Name, r.SourceURL, builtinPrefix)
				continue
			}
			parts := strings.Split(strings.TrimPrefix(r.SourceURL, builtinPrefix), "/")
			if len(parts) != 3 {
				t.Errorf("%s %q SourceURL = %q, want 3 path segments after prefix (version/kind/name)", r.Kind, r.Name, r.SourceURL)
				continue
			}
			if parts[0] == "" {
				t.Errorf("%s %q SourceURL has empty version segment", r.Kind, r.Name)
			}
			if parts[1] != string(r.Kind) {
				t.Errorf("%s %q SourceURL kind segment = %q, want %q", r.Kind, r.Name, parts[1], r.Kind)
			}
			if parts[2] != r.Name {
				t.Errorf("%s %q SourceURL name segment = %q, want %q", r.Kind, r.Name, parts[2], r.Name)
			}
		}
	}
}

func TestMandatoryBoilerplateFS(t *testing.T) {
	fsys := MandatoryBoilerplateFS()
	if fsys == nil {
		t.Fatal("MandatoryBoilerplateFS() returned nil")
	}
	data, err := fs.ReadFile(fsys, "agent-instructions-preamble.md")
	if err != nil {
		t.Fatalf("cannot read agent-instructions-preamble.md: %v", err)
	}
	if len(data) == 0 {
		t.Error("agent-instructions-preamble.md is empty")
	}
}

func assertSourceURL(t *testing.T, r BundledResource) {
	t.Helper()
	if r.Kind == storage.ResourceKindHarnessConfig {
		if !strings.HasPrefix(r.SourceURL, "git+https://github.com/GoogleCloudPlatform/scion/harnesses/") {
			t.Errorf("%s %q SourceURL = %q, want git+https://... prefix", r.Kind, r.Name, r.SourceURL)
		}
	} else if !strings.HasPrefix(r.SourceURL, "builtin://scion/") {
		t.Errorf("%s %q SourceURL = %q, want builtin://scion/... prefix", r.Kind, r.Name, r.SourceURL)
	}
}

func assertFileReadable(t *testing.T, r BundledResource, name string) {
	t.Helper()
	path := name
	if r.Root != "" && r.Root != "." {
		path = r.Root + "/" + name
	}
	data, err := fs.ReadFile(r.FS, path)
	if err != nil {
		t.Errorf("%s %q: cannot read %q: %v", r.Kind, r.Name, path, err)
		return
	}
	if len(data) == 0 {
		t.Errorf("%s %q: %q is empty", r.Kind, r.Name, path)
	}
}
