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
)

func TestCLISettings(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Test defaults (embedded)
	s, err := LoadSettings(projectScionDir)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if s.CLI == nil {
		t.Fatal("expected CLI settings to be non-nil")
	}
	if s.CLI.AutoHelp == nil {
		t.Fatal("expected CLI.AutoHelp to be non-nil")
	}
	if *s.CLI.AutoHelp != true {
		t.Errorf("expected default autohelp true, got %v", *s.CLI.AutoHelp)
	}

	// 2. Test override via UpdateSetting
	err = UpdateSetting(projectScionDir, "cli.autohelp", "false", false)
	if err != nil {
		t.Fatalf("UpdateSetting failed: %v", err)
	}

	s, err = LoadSettings(projectScionDir)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if s.CLI == nil || s.CLI.AutoHelp == nil || *s.CLI.AutoHelp != false {
		t.Errorf("expected autohelp false after update, got %v", s.CLI.AutoHelp)
	}

	// 3. The setting's value and source, as config get and list read it.
	v, err := ResolveSettingValue(projectScionDir, "cli.autohelp")
	if err != nil {
		t.Fatalf("ResolveSettingValue failed: %v", err)
	}
	if v.Value != "false" || !strings.HasPrefix(v.Source, "project ") {
		t.Errorf("ResolveSettingValue = %+v, want false from the project file", v)
	}
}
