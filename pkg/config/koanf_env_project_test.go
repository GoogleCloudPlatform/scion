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

// TestLoadSettingsIgnoringEnvProjectID checks that only the project-ID
// environment overlay is dropped (ptone/scion#3123): other SCION_ variables
// still apply.
func TestLoadSettingsIgnoringEnvProjectID(t *testing.T) {
	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("project_id: file-id\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_PROJECT_ID", "env-id")
	t.Setenv("SCION_HUB_PROJECT_ID", "env-hub-id")
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example")
	t.Chdir(tmpHome)

	withEnv, err := LoadSettings(globalDir)
	if err != nil {
		t.Fatal(err)
	}
	if withEnv.ProjectID == "file-id" {
		t.Fatalf("LoadSettings ProjectID = %q, want an env value", withEnv.ProjectID)
	}

	s, err := LoadSettingsIgnoringEnvProjectID(globalDir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProjectID != "file-id" {
		t.Errorf("ProjectID = %q, want %q", s.ProjectID, "file-id")
	}
	if s.Hub == nil || s.Hub.Endpoint != "http://hub.example" {
		t.Errorf("hub endpoint from env not applied: %+v", s.Hub)
	}
}
