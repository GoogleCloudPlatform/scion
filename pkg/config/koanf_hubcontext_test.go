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
)

func TestLoadSettings_HubEndpointFromEnvOnly(t *testing.T) {
	// Simulates a hub-connected container where SCION_HUB_ENDPOINT is set
	// via env var but no settings file contains hub.enabled or hub.endpoint.
	// LoadSettings should still populate Hub.Endpoint from the env var.

	tmpDir := t.TempDir()
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Minimal settings file — no hub section at all
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set hub env vars like a container would have
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.test:8080")
	t.Setenv("SCION_GROVE_ID", "test-grove-id")
	t.Setenv("HOME", tmpDir)

	settings, err := LoadSettings(scionDir)
	if err != nil {
		t.Fatalf("LoadSettings error: %v", err)
	}

	t.Logf("Hub is nil: %v", settings.Hub == nil)
	if settings.Hub != nil {
		t.Logf("Hub.Endpoint: %q", settings.Hub.Endpoint)
		t.Logf("Hub.Enabled: %v", settings.Hub.Enabled)
	}
	t.Logf("ProjectID: %q", settings.ProjectID)
	t.Logf("GetHubEndpoint: %q", settings.GetHubEndpoint())

	if settings.Hub == nil {
		t.Fatal("settings.Hub is nil; expected koanf to populate it from SCION_HUB_ENDPOINT env var")
	}
	if settings.Hub.Endpoint != "http://hub.test:8080" {
		t.Errorf("Hub.Endpoint = %q, want %q", settings.Hub.Endpoint, "http://hub.test:8080")
	}
	if settings.ProjectID != "test-grove-id" {
		t.Errorf("ProjectID = %q, want %q", settings.ProjectID, "test-grove-id")
	}
}

func TestLoadSettings_HubEndpointFromEnvNoSettingsFile(t *testing.T) {
	// Even more minimal: no settings file at all, just env vars.
	// This can happen when the synthetic path from FindProjectRoot doesn't
	// point to any real directory.

	tmpDir := t.TempDir()
	nonExistentPath := filepath.Join(tmpDir, "nonexistent", ".scion")

	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.test:9090")
	t.Setenv("SCION_GROVE_ID", "env-grove-id")
	t.Setenv("HOME", tmpDir)

	settings, err := LoadSettings(nonExistentPath)
	if err != nil {
		t.Fatalf("LoadSettings error: %v", err)
	}

	if settings.Hub == nil {
		t.Fatal("settings.Hub is nil; expected koanf to populate it from SCION_HUB_ENDPOINT env var")
	}
	if settings.Hub.Endpoint != "http://hub.test:9090" {
		t.Errorf("Hub.Endpoint = %q, want %q", settings.Hub.Endpoint, "http://hub.test:9090")
	}
	if settings.ProjectID != "env-grove-id" {
		t.Errorf("ProjectID = %q, want %q", settings.ProjectID, "env-grove-id")
	}
}
