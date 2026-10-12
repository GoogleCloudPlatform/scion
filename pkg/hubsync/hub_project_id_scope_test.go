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

package hubsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestEnsureHubReady_GlobalHubProjectIDDoesNotLeak reproduces the re-link
// state: the global settings are linked to a hub project, and the project's
// own settings are linked but hold no hub project ID. The hub context must
// use the project's local ID, not the global project's.
func TestEnsureHubReady_GlobalHubProjectIDDoesNotLeak(t *testing.T) {
	const localID = "0b5e6a1c-0000-4000-8000-000000000001" // setupSplitStorageProject's project-id
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_HUB_PROJECT_ID", "")
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nhub:\n  enabled: true\n  linked: true\n  project_id: \"global-hub-project-id\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scionDir, _ := setupSplitStorageProject(t, home)
	if err := config.UpdateSetting(scionDir, "hub.linked", "true", false); err != nil {
		t.Fatal(err)
	}
	server, _, registers := registeredHub(t, localID)
	withPromptIO(t, strings.NewReader(""), false)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: server.URL, SkipSync: true})
	if err != nil {
		t.Fatalf("EnsureHubReady: %v", err)
	}
	if hubCtx.ProjectID != localID {
		t.Fatalf("hub project ID = %q, want the project's own %q (not the global one)", hubCtx.ProjectID, localID)
	}
	if hubCtx.IsGlobal {
		t.Error("resolved as the global project")
	}
	if n := registers.Load(); n != 0 {
		t.Errorf("register calls = %d, want 0 (the project is registered under its own ID)", n)
	}
}

// TestRegisterProject_AlwaysWritesHubProjectID checks that the auto-link
// records hub.projectId in the project's own settings even when the hub ID
// equals the local project ID.
func TestRegisterProject_AlwaysWritesHubProjectID(t *testing.T) {
	clearHubCredentialEnv(t)
	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	withPromptIO(t, strings.NewReader(""), false)
	hubCtx := registerTestHubCtx(t, projectDir, "same-as-local-id")

	if err := registerProject(context.Background(), hubCtx, "proj", false); err != nil {
		t.Fatalf("registerProject: %v", err)
	}
	own, err := config.LoadSettingsFromDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := own.GetHubProjectID(); got != "same-as-local-id" {
		t.Fatalf("own hub.projectId = %q, want same-as-local-id", got)
	}
}

// TestRegisterProject_WritesHubProjectIDSplitStorage checks that the
// auto-link writes hub.projectId to the external directory of a git project
// with split storage, where the settings loader reads it.
func TestRegisterProject_WritesHubProjectIDSplitStorage(t *testing.T) {
	home := clearHubCredentialEnv(t)
	scionDir, extDir := setupSplitStorageProject(t, home)
	withPromptIO(t, strings.NewReader(""), false)
	hubCtx := registerTestHubCtx(t, scionDir, "split-hub-id")

	if err := registerProject(context.Background(), hubCtx, "splitrepo", false); err != nil {
		t.Fatalf("registerProject: %v", err)
	}
	ext, err := config.LoadSettingsFromDir(extDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ext.GetHubProjectID(); got != "split-hub-id" {
		t.Fatalf("external hub.projectId = %q, want split-hub-id", got)
	}
}
