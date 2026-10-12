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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// keptAgentLayout builds a project in a temp HOME and returns the project
// path to pass to delete, plus the agent dir, home and workspace path that
// GetAgent would hand to handleAgentDirWithoutConfig for that layout.
type keptAgentLayout func(t *testing.T, home, agentName string) (projectPath, agentDir, agentHome, agentWorkspace string)

func inProjectKeptLayout(t *testing.T, home, agentName string) (string, string, string, string) {
	t.Helper()
	scionDir := filepath.Join(home, "project", ".scion")
	mustMkdirAll(t, filepath.Join(scionDir, "agents"))
	agentDir := filepath.Join(scionDir, "agents", agentName)
	return scionDir, agentDir, config.GetAgentHomePath(scionDir, agentName), filepath.Join(agentDir, "workspace")
}

func globalKeptLayout(t *testing.T, home, agentName string) (string, string, string, string) {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir: %v", err)
	}
	agentsDir, err := config.GetGlobalAgentsDir()
	if err != nil {
		t.Fatalf("GetGlobalAgentsDir: %v", err)
	}
	mustMkdirAll(t, agentsDir)
	agentDir := filepath.Join(agentsDir, agentName)
	return globalDir, agentDir, filepath.Join(agentDir, "home"), filepath.Join(agentDir, "workspace")
}

func externalKeptLayout(t *testing.T, home, agentName string) (string, string, string, string) {
	t.Helper()
	scionDir := filepath.Join(home, "project", ".scion")
	mustMkdirAll(t, scionDir)
	if err := config.WriteProjectID(scionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID: %v", err)
	}
	extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir)
	if err != nil || extAgentsDir == "" {
		t.Fatalf("GetGitProjectExternalAgentsDir: dir=%q err=%v", extAgentsDir, err)
	}
	mustMkdirAll(t, extAgentsDir)
	agentDir := filepath.Join(extAgentsDir, agentName)
	// A shared-workspace agent has no in-dir workspace path.
	return scionDir, agentDir, config.GetAgentHomePath(scionDir, agentName), ""
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// TestDeleteFindsKeptAgentDir covers ptone/scion#4743: for each
// state-directory layout, a directory that stale-directory cleanup keeps
// (home data, no scion-agent.json, no agent record) is found by the local
// delete's lookup and removed by DeleteAgentFiles, as the keep error's
// "delete and re-create the agent" advice expects.
func TestDeleteFindsKeptAgentDir(t *testing.T) {
	layouts := []struct {
		name   string
		layout keptAgentLayout
	}{
		{"in-project", inProjectKeptLayout},
		{"global", globalKeptLayout},
		{"external", externalKeptLayout},
	}
	for _, tc := range layouts {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_HOST_UID", "")
			home := t.TempDir()
			t.Setenv("HOME", home)
			oldWd, _ := os.Getwd()
			t.Cleanup(func() { _ = os.Chdir(oldWd) })
			if err := os.Chdir(home); err != nil {
				t.Fatalf("chdir: %v", err)
			}

			const agentName = "kept-agent"
			projectPath, agentDir, agentHome, agentWorkspace := tc.layout(t, home, agentName)

			if found, err := LocalAgentDirExists(agentName, projectPath); err != nil || found {
				t.Fatalf("before the agent dir exists: LocalAgentDirExists = %v, %v; want false, nil", found, err)
			}

			// Home data with no agent record and no scion-agent.json: the
			// stale-directory check keeps the directory.
			mustMkdirAll(t, agentHome)
			if err := os.WriteFile(filepath.Join(agentHome, ".bash_history"), []byte("ls\n"), 0644); err != nil {
				t.Fatalf("write home file: %v", err)
			}
			mustMkdirAll(t, agentDir)
			err := handleAgentDirWithoutConfig(agentName, agentDir, agentHome, agentWorkspace)
			if !errors.Is(err, config.ErrAgentStateConflict) {
				t.Fatalf("handleAgentDirWithoutConfig = %v; want an error wrapping ErrAgentStateConflict", err)
			}
			if _, err := os.Stat(agentDir); err != nil {
				t.Fatalf("agent dir was not kept: %v", err)
			}

			found, err := LocalAgentDirExists(agentName, projectPath)
			if err != nil || !found {
				t.Fatalf("LocalAgentDirExists = %v, %v; want true, nil", found, err)
			}

			if _, err := DeleteAgentFiles(agentName, projectPath, false); err != nil {
				t.Fatalf("DeleteAgentFiles: %v", err)
			}
			if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
				t.Errorf("kept agent dir %s was not removed (stat err=%v)", agentDir, err)
			}
			if found, err := LocalAgentDirExists(agentName, projectPath); err != nil || found {
				t.Errorf("after delete: LocalAgentDirExists = %v, %v; want false, nil", found, err)
			}
		})
	}
}

// TestLocalAgentDirExistsRejectsMultiElementName checks that a name that is
// not a single path element is refused rather than looked up.
func TestLocalAgentDirExistsRejectsMultiElementName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionDir := filepath.Join(home, "project", ".scion")
	mustMkdirAll(t, filepath.Join(scionDir, "agents"))
	// A sibling directory that "../sibling" would otherwise resolve to.
	mustMkdirAll(t, filepath.Join(scionDir, "sibling"))

	for _, name := range []string{"", ".", "..", "../sibling", "a/b"} {
		found, err := LocalAgentDirExists(name, scionDir)
		if err == nil || found {
			t.Errorf("LocalAgentDirExists(%q) = %v, %v; want false and an error", name, found, err)
		}
	}
}
