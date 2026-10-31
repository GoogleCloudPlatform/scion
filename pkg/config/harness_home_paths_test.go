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
	"strings"
	"testing"
	"testing/fstest"
)

// homePathFields are the config.yaml fields that name a path in the agent
// home.
var homePathFields = []string{"system_prompt_file", "instructions_file", "skills_dir", "config_dir"}

// nonLocalHomePaths are values that do not name a path inside the agent
// home.
var nonLocalHomePaths = map[string]string{
	"parent":          "../outside",
	"nested parent":   "a/../../outside",
	"absolute":        "/etc/outside",
	"home itself":     ".",
	"cleans to home":  "a/..",
	"trailing parent": "a/b/../..",
}

// A harness-config whose agent-home path field is not a local relative path
// does not load, through either loader, and the error names the field but
// not the value.
func TestHarnessConfigHomePathFieldsMustBeLocal(t *testing.T) {
	for _, field := range homePathFields {
		for name, val := range nonLocalHomePaths {
			t.Run(field+"/"+name, func(t *testing.T) {
				cfg := sidecarTestConfig + field + ": '" + val + "'\n"
				dir := writeSidecarTestDir(t, cfg, "")
				_, dirErr := LoadHarnessConfigDir(dir)
				_, filesErr := LoadHarnessConfigFromFiles(filepath.Base(dir), dir, dirFileReader(dir))
				for loader, err := range map[string]error{"LoadHarnessConfigDir": dirErr, "LoadHarnessConfigFromFiles": filesErr} {
					if err == nil {
						t.Fatalf("%s accepted %s %q", loader, field, val)
					}
					if !strings.Contains(err.Error(), field) {
						t.Errorf("%s error %q does not name %s", loader, err, field)
					}
					if strings.Contains(val, "/") && strings.Contains(err.Error(), val) {
						t.Errorf("%s error %q carries the value", loader, err)
					}
				}
			})
		}
	}
}

// Seeding a harness-config from a source tree refuses a config_dir that is
// not a local relative path, and creates nothing outside the target.
func TestSeedHarnessConfigFromDirRefusesNonLocalConfigDir(t *testing.T) {
	base := t.TempDir()
	targetDir := filepath.Join(base, "harness-configs", "hc")
	src := fstest.MapFS{
		"hc/config.yaml": &fstest.MapFile{Data: []byte("harness: hc\nimage: img:test\nconfig_dir: ../../../escaped\n")},
	}
	if err := SeedHarnessConfigFromDir(targetDir, src, "hc", false); err == nil {
		t.Fatal("SeedHarnessConfigFromDir accepted a config_dir outside the home")
	}
	if _, err := os.Lstat(filepath.Join(base, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("a directory was created outside the harness-config home: %v", err)
	}
}

// Local relative values, nested or not, still load.
func TestHarnessConfigHomePathFieldsLocalValuesLoad(t *testing.T) {
	cfg := sidecarTestConfig +
		"system_prompt_file: .decl/system.md\n" +
		"instructions_file: AGENTS.md\n" +
		"skills_dir: .decl/skills\n" +
		"config_dir: ./.decl\n"
	dir := writeSidecarTestDir(t, cfg, "")
	hc, err := LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	if hc.Config.SystemPromptFile != ".decl/system.md" || hc.Config.ConfigDir != "./.decl" {
		t.Fatalf("unexpected entry: %+v", hc.Config)
	}
	if err := ValidateHarnessHomePaths(HarnessConfigEntry{}); err != nil {
		t.Fatalf("empty fields refused: %v", err)
	}
}

// ValidateHarnessHomePaths, which harness.Resolve also runs on the merged
// entry, refuses a non-local value in each field of a hand-built entry and
// accepts local relative ones.
func TestValidateHarnessHomePathsOnBuiltEntry(t *testing.T) {
	set := map[string]func(*HarnessConfigEntry, string){
		"system_prompt_file": func(e *HarnessConfigEntry, v string) { e.SystemPromptFile = v },
		"instructions_file":  func(e *HarnessConfigEntry, v string) { e.InstructionsFile = v },
		"skills_dir":         func(e *HarnessConfigEntry, v string) { e.SkillsDir = v },
		"config_dir":         func(e *HarnessConfigEntry, v string) { e.ConfigDir = v },
	}
	for _, field := range homePathFields {
		for name, val := range nonLocalHomePaths {
			var e HarnessConfigEntry
			set[field](&e, val)
			err := ValidateHarnessHomePaths(e)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s %s (%q): error = %v, want one naming the field", field, name, val, err)
			}
		}
		for _, val := range []string{"agents.md", ".decl/sub/file.md", "./.decl"} {
			var e HarnessConfigEntry
			set[field](&e, val)
			if err := ValidateHarnessHomePaths(e); err != nil {
				t.Errorf("%s %q refused: %v", field, val, err)
			}
		}
	}
}
