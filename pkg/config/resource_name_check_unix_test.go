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

//go:build unix

package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const outsideMarker = "outside-marker-content"

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestFindHarnessConfigDirDoesNotFollowLinks(t *testing.T) {
	// A linked project-tier entry never supplies content: the lookup skips
	// it and resolves from the next tier (global), or finds nothing.
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, projectPath, outside string)
	}{
		{"linked harness-config dir", func(t *testing.T, projectPath, outside string) {
			mustSymlink(t, outside, filepath.Join(projectPath, harnessConfigsDirName, "hc"))
		}},
		{"linked harness-configs dir", func(t *testing.T, projectPath, outside string) {
			parent := filepath.Join(filepath.Dir(outside), "outside-parent")
			writeNamedHarnessConfigDir(t, filepath.Join(parent, "hc"), "outside-image:1")
			mustSymlink(t, parent, filepath.Join(projectPath, harnessConfigsDirName))
		}},
		{"linked config.yaml", func(t *testing.T, projectPath, outside string) {
			dir := filepath.Join(projectPath, harnessConfigsDirName, "hc")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, filepath.Join(outside, "config.yaml"), filepath.Join(dir, "config.yaml"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			projectPath := filepath.Join(tmp, "project", DotScion)
			outside := filepath.Join(tmp, "outside")
			writeNamedHarnessConfigDir(t, outside, "outside-image:1")
			tc.plant(t, projectPath, outside)

			logs := captureDefaultLog(t)
			if hc, err := FindHarnessConfigDir("hc", projectPath); err == nil {
				t.Fatalf("FindHarnessConfigDir loaded %q (image %q) through a link", hc.Path, hc.Config.Image)
			}
			// The skip warning names the tier only, never a path.
			if !strings.Contains(logs.String(), "tier=project") {
				t.Errorf("no skip warning naming the project tier; log: %q", logs.String())
			}
			if strings.Contains(logs.String(), tmp) {
				t.Errorf("skip warning contains a path: %q", logs.String())
			}

			writeNamedHarnessConfigDir(t, filepath.Join(tmp, GlobalDir, harnessConfigsDirName, "hc"), "global-image:1")
			hc, err := FindHarnessConfigDir("hc", projectPath)
			if err != nil {
				t.Fatalf("FindHarnessConfigDir: %v", err)
			}
			if hc.Config.Image != "global-image:1" {
				t.Errorf("image = %q; want the global tier's (the linked project tier must be skipped)", hc.Config.Image)
			}
		})
	}
}

func TestFindHarnessConfigDirTemplateTierDoesNotFollowLinks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	tplPath := filepath.Join(tmp, "tpl")
	outside := filepath.Join(tmp, "outside")
	writeNamedHarnessConfigDir(t, outside, "outside-image:1")
	mustSymlink(t, outside, filepath.Join(tplPath, harnessConfigsDirName, "hc"))

	if hc, err := FindHarnessConfigDir("hc", "", tplPath); err == nil {
		t.Fatalf("FindHarnessConfigDir loaded image %q through a linked template-tier dir", hc.Config.Image)
	}
}

// captureDefaultLog routes the default slog logger to a buffer for the test.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A template directory reached through a symbolic link never supplies its
// bundled harness-config: not when the template directory itself is the
// link, not when the templates directory is, under the project's .scion or
// the global directory, nor for a template outside both (walked from its
// parent).
func TestFindHarnessConfigDirLinkedTemplateDirNeverSupplies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, tmp, projectPath, outsideParent string) string
	}{
		{"linked project template dir", func(t *testing.T, tmp, projectPath, outsideParent string) string {
			tpl := filepath.Join(projectPath, "templates", "tpl")
			mustSymlink(t, filepath.Join(outsideParent, "tpl"), tpl)
			return tpl
		}},
		{"linked project templates dir", func(t *testing.T, tmp, projectPath, outsideParent string) string {
			mustSymlink(t, outsideParent, filepath.Join(projectPath, "templates"))
			return filepath.Join(projectPath, "templates", "tpl")
		}},
		{"linked global template dir", func(t *testing.T, tmp, projectPath, outsideParent string) string {
			tpl := filepath.Join(tmp, GlobalDir, "templates", "tpl")
			mustSymlink(t, filepath.Join(outsideParent, "tpl"), tpl)
			return tpl
		}},
		{"linked template dir outside the roots", func(t *testing.T, tmp, projectPath, outsideParent string) string {
			tpl := filepath.Join(tmp, "elsewhere", "tpl")
			mustSymlink(t, filepath.Join(outsideParent, "tpl"), tpl)
			return tpl
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			projectPath := filepath.Join(tmp, "project", DotScion)
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}
			outsideParent := filepath.Join(tmp, "outside")
			writeNamedHarnessConfigDir(t, filepath.Join(outsideParent, "tpl", harnessConfigsDirName, "hc"), "outside-image:1")
			tplPath := tc.plant(t, tmp, projectPath, outsideParent)

			if hc, err := FindHarnessConfigDir("hc", projectPath, tplPath); err == nil {
				t.Fatalf("FindHarnessConfigDir loaded image %q from a linked template dir", hc.Config.Image)
			}
			if hc, err := ResolveHarnessConfigDir("", "hc", projectPath, tplPath); err == nil {
				t.Fatalf("ResolveHarnessConfigDir loaded image %q from a linked template dir", hc.Config.Image)
			}
		})
	}

	// Control: a real template dir under the project's .scion supplies its
	// bundled harness-config.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath := filepath.Join(tmp, "project", DotScion)
	tpl := filepath.Join(projectPath, "templates", "tpl")
	writeNamedHarnessConfigDir(t, filepath.Join(tpl, harnessConfigsDirName, "hc"), "tpl-image:1")
	hc, err := FindHarnessConfigDir("hc", projectPath, tpl)
	if err != nil || hc.Config.Image != "tpl-image:1" || hc.Source != HarnessConfigSourceTemplateBundled {
		t.Fatalf("FindHarnessConfigDir(real template) = %+v, %v", hc, err)
	}
}

func writeAgentConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("default_harness_config: "+outsideMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRefusesLinkedAgentConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Run("linked config file", func(t *testing.T) {
		tmp := t.TempDir()
		outside := filepath.Join(tmp, "outside", "scion-agent.yaml")
		writeAgentConfig(t, outside)
		tplDir := filepath.Join(tmp, "templates", "tpl")
		mustSymlink(t, outside, filepath.Join(tplDir, "scion-agent.yaml"))

		cfg, err := (&Template{Path: tplDir}).LoadConfig()
		if !errors.Is(err, ErrAgentConfigRefused) {
			t.Fatalf("LoadConfig = %+v, %v; want ErrAgentConfigRefused", cfg, err)
		}
		if strings.Contains(err.Error(), tmp) || strings.Contains(err.Error(), outsideMarker) {
			t.Errorf("error leaks a path or content: %v", err)
		}
	})
	t.Run("linked template dir below its root", func(t *testing.T) {
		tmp := t.TempDir()
		t.Chdir(tmp)
		t.Setenv("HOME", tmp)
		outsideDir := filepath.Join(tmp, "outside")
		writeAgentConfig(t, filepath.Join(outsideDir, "scion-agent.yaml"))
		globalTemplates := filepath.Join(tmp, GlobalDir, "templates")
		mustSymlink(t, outsideDir, filepath.Join(globalTemplates, "tpl"))

		tpl, err := FindTemplate("tpl")
		if err != nil {
			t.Fatalf("FindTemplate: %v", err)
		}
		cfg, err := tpl.LoadConfig()
		if !errors.Is(err, ErrAgentConfigRefused) {
			t.Fatalf("LoadConfig = %+v, %v; want ErrAgentConfigRefused", cfg, err)
		}
	})
	t.Run("hard-linked config file", func(t *testing.T) {
		tmp := t.TempDir()
		outside := filepath.Join(tmp, "outside", "scion-agent.yaml")
		writeAgentConfig(t, outside)
		tplDir := filepath.Join(tmp, "tpl")
		if err := os.MkdirAll(tplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(outside, filepath.Join(tplDir, "scion-agent.yaml")); err != nil {
			t.Skipf("hard links not supported here: %v", err)
		}
		if _, err := (&Template{Path: tplDir}).LoadConfig(); !errors.Is(err, ErrAgentConfigRefused) {
			t.Fatalf("LoadConfig err = %v; want ErrAgentConfigRefused", err)
		}
	})
}

func TestLoadConfigRegularFilesStillLoad(t *testing.T) {
	for _, name := range agentConfigFileNames {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			content := "default_harness_config: hc-one\n"
			if strings.HasSuffix(name, ".json") {
				content = `{"default_harness_config": "hc-one"}`
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := (&Template{Path: dir}).LoadConfig()
			if err != nil || cfg.DefaultHarnessConfig != "hc-one" {
				t.Fatalf("LoadConfig = %+v, %v", cfg, err)
			}
			if GetScionAgentConfigPath(dir) != filepath.Join(dir, name) {
				t.Errorf("GetScionAgentConfigPath = %q", GetScionAgentConfigPath(dir))
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		cfg, err := (&Template{Path: t.TempDir()}).LoadConfig()
		if err != nil || cfg == nil || cfg.DefaultHarnessConfig != "" {
			t.Fatalf("LoadConfig = %+v, %v; want an empty config", cfg, err)
		}
	})
}

func TestGetScionAgentConfigPathIgnoresLinks(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside", "scion-agent.yaml")
	writeAgentConfig(t, outside)
	dir := filepath.Join(tmp, "agent")
	mustSymlink(t, outside, filepath.Join(dir, "scion-agent.yaml"))
	if p := GetScionAgentConfigPath(dir); p != "" {
		t.Errorf("GetScionAgentConfigPath = %q; want \"\" for a linked config file", p)
	}
	if ScionAgentConfigExists(dir) {
		t.Error("ScionAgentConfigExists = true for a linked config file")
	}

	// The first entry decides, as in Template.LoadConfig: a linked
	// scion-agent.yaml next to a regular scion-agent.json is refused, not
	// skipped in favour of the .json.
	t.Run("linked yaml before regular json", func(t *testing.T) {
		d := filepath.Join(tmp, "first-entry")
		mustSymlink(t, outside, filepath.Join(d, "scion-agent.yaml"))
		if err := os.WriteFile(filepath.Join(d, "scion-agent.json"), []byte(`{"default_harness_config":"hc-one"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if p := GetScionAgentConfigPath(d); p != "" {
			t.Errorf("GetScionAgentConfigPath = %q; want \"\" (the first entry is refused)", p)
		}
		if st := AgentConfigEntryState(d); st != AgentConfigRefused {
			t.Errorf("AgentConfigEntryState = %v; want AgentConfigRefused", st)
		}
		if _, err := (&Template{Path: d}).LoadConfig(); !errors.Is(err, ErrAgentConfigRefused) {
			t.Errorf("LoadConfig err = %v; want ErrAgentConfigRefused", err)
		}
	})

	// A hard-linked config file is refused by the probe, as by LoadConfig.
	t.Run("hard-linked config", func(t *testing.T) {
		d := filepath.Join(tmp, "hard-link")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(outside, filepath.Join(d, "scion-agent.yaml")); err != nil {
			t.Skipf("hard links not supported here: %v", err)
		}
		if p := GetScionAgentConfigPath(d); p != "" {
			t.Errorf("GetScionAgentConfigPath = %q; want \"\" for a hard-linked file", p)
		}
		if st := AgentConfigEntryState(d); st != AgentConfigRefused {
			t.Errorf("AgentConfigEntryState = %v; want AgentConfigRefused", st)
		}
	})
}

func TestProjectSettingsFileLinkRefused(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Chdir(tmp)
	outside := filepath.Join(tmp, "outside", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("default_template: "+outsideMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmp, "project", DotScion)
	mustSymlink(t, outside, filepath.Join(projectDir, "settings.yaml"))
	s, err := LoadProjectSettingsFromDir(projectDir)
	if !errors.Is(err, ErrSettingsFileRefused) {
		t.Fatalf("LoadProjectSettingsFromDir(project) = %+v, %v; want ErrSettingsFileRefused", s, err)
	}
	if strings.Contains(err.Error(), tmp) || strings.Contains(err.Error(), outsideMarker) {
		t.Errorf("error leaks a path or content: %v", err)
	}

	// Control: the global tier may link its settings file (dotfiles).
	globalDir := filepath.Join(tmp, GlobalDir)
	mustSymlink(t, outside, filepath.Join(globalDir, "settings.yaml"))
	s, err = LoadSettingsFromDir(globalDir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir(global): %v", err)
	}
	if s.DefaultTemplate != outsideMarker {
		t.Errorf("global DefaultTemplate = %q; want the linked file's value", s.DefaultTemplate)
	}

	// Control: a regular project settings file loads.
	regularDir := filepath.Join(tmp, "project2", DotScion)
	if err := os.MkdirAll(regularDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regularDir, "settings.yaml"), []byte("default_template: tpl-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadProjectSettingsFromDir(regularDir); err != nil || s.DefaultTemplate != "tpl-a" {
		t.Fatalf("LoadProjectSettingsFromDir(regular) = %+v, %v", s, err)
	}
}

// The server's --config directory is not a project layer: a settings.yaml
// there that is a symbolic link (as a Secret or ConfigMap mounted as a
// directory gives) still loads, and both readers of it (LoadGlobalConfig and
// the bootstrap koanf layer) agree.
func TestServerConfigDirSettingsLinkStillLoads(t *testing.T) {
	_, cfgDir := writeConfigPathFixture(t,
		"schema_version: \"1\"\nimage_registry: global.example.com\n",
		"schema_version: \"1\"\nimage_registry: cfg.example.com\nserver:\n  hub:\n    stalled_threshold: 9m\n")
	target := filepath.Join(filepath.Dir(cfgDir), "mounted", "settings.yaml")
	data, err := os.ReadFile(filepath.Join(cfgDir, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfgDir, "settings.yaml")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, target, filepath.Join(cfgDir, "settings.yaml"))

	assertStalledThresholdAgrees(t, cfgDir, "9m0s")
	if got := LoadBootstrapKoanfWithConfigPath(cfgDir).String("image_registry"); got != "cfg.example.com" {
		t.Errorf("image_registry = %q, want cfg.example.com from the linked --config file", got)
	}
	if s, err := LoadSettingsFromDir(cfgDir); err != nil || s == nil {
		t.Errorf("LoadSettingsFromDir(--config dir) = %v, %v; want the linked file loaded", s, err)
	}
}
