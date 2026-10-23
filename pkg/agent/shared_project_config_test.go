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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

const sharedCfgProjectID = "0f9e8d7c-6b5a-4321-8fed-cba987654321"

// sharedConfigProject creates a hub-seeded shared project root
// (<share>/projects/<id>) under a temp HOME and returns the root and HOME.
func sharedConfigProject(t *testing.T) (root, home string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	root = filepath.Join(t.TempDir(), "share-a", "projects", sharedCfgProjectID)
	scionDir := filepath.Join(root, config.DotScion)
	mustMkdirAll(t, scionDir)
	for name, content := range map[string]string{
		projectkeys.ProjectIDFile:   sharedCfgProjectID + "\n",
		projectkeys.ProjectSlugFile: "web\n",
		"settings.yaml":             "schema_version: \"1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, home
}

// The external agents root of a shared project holds a worktree-mode
// agent's state too, and that alone does not make the agent a
// shared-workspace agent.
func TestEffectiveSharedWorkspace_SharedProjectConfig(t *testing.T) {
	root, home := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	dir, shared, err := AgentStateDir(scionDir, "a1", false, sharedCfgProjectID, true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, "web__0f9e8d7c", config.DotScion, "agents", "a1")
	if dir != want || shared {
		t.Fatalf("AgentStateDir = (%q, shared=%v), want (%q, false)", dir, shared, want)
	}
	mustMkdirAll(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, shared, _ := AgentStateDir(scionDir, "a1", false, sharedCfgProjectID, true); shared {
		t.Error("state in the forced external root must not flip the agent to shared-workspace")
	}
}

// An agent provisioned before the configuration moved keeps its legacy
// in-project dir: it is found for delete and removed with the agent, so a
// later agent of the same name never resolves to it.
func TestDeleteAgentFiles_SharedProjectLegacyDir(t *testing.T) {
	for _, hubID := range []string{sharedCfgProjectID, ""} {
		t.Run("hub id "+hubID, func(t *testing.T) { testDeleteSharedProjectLegacyDir(t, hubID) })
	}
}

func testDeleteSharedProjectLegacyDir(t *testing.T, hubID string) {
	root, home := sharedConfigProject(t)
	legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedCfgProjectID); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, legacy)
	if err := os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	exists, err := LocalAgentDirExists("old", root)
	if err != nil || !exists {
		t.Fatalf("LocalAgentDirExists = (%v, %v), want the legacy dir found", exists, err)
	}
	if _, err := DeleteAgentFilesForHubProject("old", root, hubID, false); err != nil {
		t.Fatalf("DeleteAgentFilesForHubProject: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy agent dir should be removed with the agent (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, config.DotScion, projectkeys.ProjectIDFile)); err != nil {
		t.Fatalf("the shared project configuration must be left alone: %v", err)
	}
	if got := config.ResolveAgentDir(filepath.Join(root, config.DotScion), "old"); got == legacy {
		t.Error("a new agent of the same name must not resolve to the removed legacy dir")
	}
}
