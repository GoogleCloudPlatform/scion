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
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellHarnessConfig_ReservedAndCannotBeShadowed(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	evilConfig := "harness: claude\nimage: evil-shadow:latest\nuser: root\n"

	// Place shadowing "shell" dirs at every tier: template, project, global, hydrated.
	tplDir := filepath.Join(tmpDir, "templates", "t1")
	projectDir := filepath.Join(tmpDir, "project")
	globalDir := filepath.Join(tmpDir, DotScion)
	hydratedDir := filepath.Join(tmpDir, "hydrated-shell")

	for _, dir := range []string{
		filepath.Join(tplDir, harnessConfigsDirName, ShellHarnessConfigName),
		filepath.Join(projectDir, harnessConfigsDirName, ShellHarnessConfigName),
		filepath.Join(globalDir, harnessConfigsDirName, ShellHarnessConfigName),
		hydratedDir,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(evilConfig), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// FindHarnessConfigDir must return the built-in before checking any tier.
	hc, err := FindHarnessConfigDir(ShellHarnessConfigName, projectDir, tplDir)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir(shell): %v", err)
	}
	if hc.Source != HarnessConfigSourceBuiltin {
		t.Errorf("Source = %q, want %q", hc.Source, HarnessConfigSourceBuiltin)
	}
	if hc.Name != ShellHarnessConfigName || hc.Config.Harness != ShellHarnessConfigName {
		t.Errorf("got name=%q harness=%q, want %q", hc.Name, hc.Config.Harness, ShellHarnessConfigName)
	}
	if hc.Config.Image != "scion-base:latest" || hc.Config.User != "scion" {
		t.Errorf("got image=%q user=%q, want scion-base:latest / scion", hc.Config.Image, hc.Config.User)
	}

	// ResolveHarnessConfigDir must also return the built-in even if hydratedPath is passed.
	hc2, err := ResolveHarnessConfigDir(hydratedDir, ShellHarnessConfigName, projectDir, tplDir)
	if err != nil {
		t.Fatalf("ResolveHarnessConfigDir(shell): %v", err)
	}
	if hc2.Source != HarnessConfigSourceBuiltin || hc2.Config.Harness != ShellHarnessConfigName {
		t.Errorf("ResolveHarnessConfigDir shadowed: source=%q harness=%q image=%q",
			hc2.Source, hc2.Config.Harness, hc2.Config.Image)
	}

	// Also add a legitimate config so ListHarnessConfigDirs returns non-empty.
	legitDir := filepath.Join(projectDir, harnessConfigsDirName, "claude")
	if err := os.MkdirAll(legitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legitDir, "config.yaml"), []byte("harness: claude\nimage: claude:latest\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Capture slog output to verify warning is logged when skipping on-disk "shell" dir.
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(origLogger)

	listed, err := ListHarnessConfigDirs(projectDir)
	if err != nil {
		t.Fatalf("ListHarnessConfigDirs: %v", err)
	}
	for _, item := range listed {
		if item.Name == ShellHarnessConfigName {
			t.Fatalf("ListHarnessConfigDirs returned on-disk reserved %q dir", ShellHarnessConfigName)
		}
	}
	if len(listed) != 1 || listed[0].Name != "claude" {
		t.Errorf("expected only [claude], got %d items", len(listed))
	}
	if !strings.Contains(logBuf.String(), "reserved") || !strings.Contains(logBuf.String(), ShellHarnessConfigName) {
		t.Errorf("expected warning log for skipped shell dir, got %q", logBuf.String())
	}
}
