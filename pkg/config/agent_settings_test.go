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

func TestLoadAgentSettings(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scion-config-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "settings.json")
	content := `{
		"apiKey": "test-key",
		"security": {
			"auth": {
				"selectedType": "api-key"
			}
		},
		"tools": {
			"sandbox": "docker"
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadAgentSettings(path)
	if err != nil {
		t.Fatalf("LoadAgentSettings failed: %v", err)
	}

	if s.ApiKey != "test-key" {
		t.Errorf("Expected apiKey test-key, got %s", s.ApiKey)
	}
	if s.Security.Auth.SelectedType != "api-key" {
		t.Errorf("Expected selectedType api-key, got %s", s.Security.Auth.SelectedType)
	}
	if s.Tools.Sandbox != "docker" {
		t.Errorf("Expected sandbox docker, got %v", s.Tools.Sandbox)
	}

	// Test invalid JSON
	if err := os.WriteFile(path, []byte(`{invalid}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadAgentSettings(path)
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}

	// Test nonexistent file
	_, err = LoadAgentSettings(filepath.Join(tmpDir, "nonexistent.json"))
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestGetAgentSettings(t *testing.T) {
	// This test depends on the environment, so we just check if it returns without crashing
	// or if it returns an error that is not "user home directory not found"
	s, err := GetAgentSettings()
	if err != nil {
		// It's okay if it fails if the file doesn't exist on the host
		return
	}
	if s == nil {
		t.Fatal("GetAgentSettings returned nil without error")
	}
}
