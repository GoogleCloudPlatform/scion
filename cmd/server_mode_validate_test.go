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

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// An unknown server.mode, from settings.yaml or SCION_SERVER_MODE, stops
// server startup instead of silently meaning workstation.
func TestLoadAndReconcileConfig_RejectsUnknownServerMode(t *testing.T) {
	for _, tc := range []struct {
		name, file, env string
	}{
		{"settings.yaml typo", "schema_version: \"1\"\nserver:\n  mode: Hosted\n", ""},
		{"env typo", "schema_version: \"1\"\n", "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("SCION_SERVER_MODE", tc.env)
			}
			_, err := loadAndReconcileConfig(serverStartCmd)
			if err == nil || !strings.Contains(err.Error(), "invalid server.mode") {
				t.Fatalf("loadAndReconcileConfig err = %v, want an invalid server.mode error", err)
			}
		})
	}
}

// The daemon path reads server.mode from settings.yaml through
// LoadServerMode; the same validation applies to its result.
func TestLoadServerMode_TypoFailsValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("server:\n  mode: prod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateServerMode(config.LoadServerMode()); err == nil {
		t.Error("a server.mode typo must fail validation on the daemon path")
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("server:\n  mode: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateServerMode(config.LoadServerMode()); err != nil {
		t.Errorf("legacy production must stay valid: %v", err)
	}
}
